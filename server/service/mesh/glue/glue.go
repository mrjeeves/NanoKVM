// Package glue wires the CGO-free mesh package to the KVM's on-device hardware:
// a VideoSource over the libkvm H.264 encoder (common) and an InputSink over the
// HID gadget (service/hid). It lives outside the mesh package precisely because
// it imports CGO/libkvm and the HID gadget — the mesh package must build and
// test on host amd64, so it reaches these only through the injected interfaces
// this package implements.
package glue

import (
	"math"
	"sync"

	"NanoKVM-Server/common"
	"NanoKVM-Server/service/hid"
	"NanoKVM-Server/service/mesh"
)

// ---- VideoSource ------------------------------------------------------------

// videoSource adapts common.GetKvmVision()/common.GetScreen() to mesh.VideoSource.
type videoSource struct{}

// NewVideoSource builds the on-device native display encoder.
func NewVideoSource() mesh.VideoSource { return videoSource{} }

// Params snapshots the live capture geometry (the same fields the WebRTC pump
// reads from common.GetScreen between frames, so a Tune reshapes both streams).
func (videoSource) Params() mesh.VideoParams {
	s := common.GetScreen()
	return mesh.VideoParams{
		Width:   int(s.Width),
		Height:  int(s.Height),
		FPS:     s.FPS,
		BitRate: int(s.BitRate),
	}
}

// ReadH264 returns one Annex-B access unit — the very bytes the WebRTC path
// feeds Pion, no re-encode.
func (videoSource) ReadH264(width, height, bitrate int) ([]byte, int) {
	return common.GetKvmVision().ReadH264(uint16(width), uint16(height), uint16(bitrate))
}

// SetFps requests a capture frame rate (validated by common.SetScreen).
func (videoSource) SetFps(fps int) {
	common.SetScreen("fps", fps)
}

// Tune applies a viewer's constraints to the shared screen params (best-effort;
// nil fields are left unchanged). max_edge is translated to the nearest
// resolution via common.ResolutionMap; bitrate rides the "quality" setter's
// >100 == bitrate rule; fps is validated by common.SetScreen.
func (videoSource) Tune(maxEdge, bitrate, fps *uint32) {
	if maxEdge != nil {
		if h, ok := resolutionForMaxEdge(*maxEdge); ok {
			common.SetScreen("resolution", h)
		}
	}
	if bitrate != nil {
		common.SetScreen("quality", int(*bitrate))
	}
	if fps != nil {
		common.SetScreen("fps", int(*fps))
	}
}

// ForceIDR is a best-effort no-op: libkvm exposes no "keyframe now" primitive,
// but it re-emits SPS+PPS+IDR at every GOP boundary (screen.GOP frames), so a
// refreshing viewer recovers on the next GOP.
func (videoSource) ForceIDR() {}

// resolutionForMaxEdge picks the largest resolution whose width (its longest
// edge) fits within maxEdge, using common.ResolutionMap (height → width).
func resolutionForMaxEdge(maxEdge uint32) (int, bool) {
	best := -1
	for h, w := range common.ResolutionMap {
		if h == 0 {
			continue
		}
		if uint32(w) <= maxEdge && int(h) > best {
			best = int(h)
		}
	}
	if best < 0 {
		return 0, false
	}
	return best, true
}

// ---- InputSink --------------------------------------------------------------

// maxKeys is the HID boot-keyboard rollover limit (keyboard.ts MAX_KEYS).
const maxKeys = 6

// inputSink adapts a normalized mesh.InputAction stream to HID reports, porting
// the browser's report construction (lib/keyboard.ts + lib/mouse.ts): it tracks
// pressed modifiers/keys to build the 8-byte keyboard report (hidg0), and mouse
// button state + last absolute position to build the 6-byte absolute-mouse
// report (hidg2). Writes go through the same hid.Hid singleton the /api/ws path
// uses; the device mutexes serialize us against it.
type inputSink struct {
	hid *hid.Hid

	mu       sync.Mutex
	modifier byte
	keys     []string // pressed non-modifier codes, in press order (≤ maxKeys)
	buttons  byte
	lastX    float64 // last absolute X, normalized 0..1
	lastY    float64
}

// NewInputSink builds the on-device native HID sink. HID devices are opened
// lazily on first write (hid.writeHID reopens a nil descriptor), so this never
// touches /dev/hidg* until an input route actually delivers an event.
func NewInputSink() mesh.InputSink {
	return &inputSink{hid: hid.GetHid(), lastX: 0.5, lastY: 0.5}
}

// Apply translates one action to a HID report and writes it.
func (s *inputSink) Apply(a mesh.InputAction) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch a.Kind {
	case mesh.InputActionMouseMove:
		s.lastX = clamp01(a.X)
		s.lastY = clamp01(a.Y)
		s.hid.WriteHid2(buildAbsoluteReport(s.buttons, s.lastX, s.lastY, 0))

	case mesh.InputActionMouseButton:
		bit := mouseButtonBit(a.Button)
		if bit == 0 {
			return
		}
		if a.Down {
			s.buttons |= bit
		} else {
			s.buttons &^= bit
		}
		s.hid.WriteHid2(buildAbsoluteReport(s.buttons, s.lastX, s.lastY, 0))

	case mesh.InputActionWheel:
		wheel := clampInt(int(math.Round(a.DY)), -127, 127)
		s.hid.WriteHid2(buildAbsoluteReport(s.buttons, s.lastX, s.lastY, wheel))

	case mesh.InputActionKey:
		s.applyKey(a)
	}
}

// applyKey updates the tracked keyboard state and writes the 8-byte report.
func (s *inputSink) applyKey(a mesh.InputAction) {
	code := resolveCode(a.Code, a.Key)
	if code == "" {
		return
	}
	if bit, ok := ModifierMap[code]; ok {
		if a.Down {
			s.modifier |= bit
		} else {
			s.modifier &^= bit
		}
	} else if _, ok := KeycodeMap[code]; ok {
		if a.Down {
			if !containsCode(s.keys, code) && len(s.keys) < maxKeys {
				s.keys = append(s.keys, code)
			}
		} else {
			s.keys = removeCode(s.keys, code)
		}
	} else {
		return // unmapped code — nothing to press
	}
	s.hid.WriteHid0(s.buildKeyReport())
}

// buildKeyReport builds the 8-byte HID keyboard report (keyboard.ts buildReport):
// [modifier, 0x00, keycode0..keycode5].
func (s *inputSink) buildKeyReport() []byte {
	report := make([]byte, 8)
	report[0] = s.modifier
	i := 2
	for _, code := range s.keys {
		if i >= 8 {
			break
		}
		report[i] = KeycodeMap[code]
		i++
	}
	return report
}

// mouseButtonBit maps a normalized button index (0=left,1=middle,2=right) to its
// HID buttons-byte bit (mouse.ts getMouseButtonBit: left=1<<0, middle=1<<2,
// right=1<<1).
func mouseButtonBit(button uint8) byte {
	switch button {
	case 0:
		return 1 << 0 // left
	case 1:
		return 1 << 2 // middle
	case 2:
		return 1 << 1 // right
	default:
		return 0
	}
}

// buildAbsoluteReport builds the 6-byte absolute-mouse report (mouse.ts
// MouseReportAbsolute.buildReport) for hidg2:
// [buttons, xLo, xHi, yLo, yHi, wheel], x/y as 15-bit little-endian.
func buildAbsoluteReport(buttons byte, x, y float64, wheel int) []byte {
	hx := scaleAxis(x)
	hy := scaleAxis(y)
	w := clampInt(wheel, -127, 127)
	return []byte{
		buttons,
		byte(hx & 0xff), byte((hx >> 8) & 0xff),
		byte(hy & 0xff), byte((hy >> 8) & 0xff),
		byte(w & 0xff),
	}
}

// scaleAxis maps a normalized 0..1 coordinate to the device's absolute range,
// exactly as the browser does (getCoordinate: floor(0x7fff * v) + 1).
func scaleAxis(v float64) int {
	v = clamp01(v)
	return int(float64(0x7fff)*v) + 1
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func containsCode(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func removeCode(xs []string, want string) []string {
	out := xs[:0]
	for _, x := range xs {
		if x != want {
			out = append(out, x)
		}
	}
	return out
}
