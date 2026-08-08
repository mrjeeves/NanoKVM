package utils

import (
	"os"

	log "github.com/sirupsen/logrus"
)

// Both are var, not const, only so the tests can point them somewhere
// writable; nothing in the server reassigns them.
var (
	HDMIDisableFile = "/etc/kvm/hdmi_disable"
	// hdmiClearedMarker records that the one-shot below has run. It sits beside
	// the flag it clears, so no caller has to thread a state directory in.
	hdmiClearedMarker = "/etc/kvm/.hdmi_disable_cleared"
)

func PersistHDMIDisabled() {
	f, err := os.OpenFile(HDMIDisableFile, os.O_CREATE|os.O_RDONLY, 0644)
	if err != nil {
		log.Error("failed to create hdmi disable file:", err)
		return
	}
	f.Close()
}

func PersistHDMIEnabled() {
	if err := os.Remove(HDMIDisableFile); err != nil {
		log.Error("failed to remove hdmi disable file:", err)
		return
	}
}

func IsHdmiDisabled() bool {
	if _, err := os.Stat(HDMIDisableFile); err != nil {
		if os.IsNotExist(err) {
			return false // HDMI is enabled
		}
		log.Error("failed to check hdmi disable file:", err)
		return false // Assume HDMI is enabled on error
	}
	return true // HDMI is disabled
}

// ClearHDMIDisabledOnce removes a persisted HDMI disable flag exactly once, on
// the first start after this ships, and never again.
//
// On-demand HDMI (reverted) left devices with their picture off, and a device
// that also carries this flag boots dark for good — nothing on screen says why,
// and the fix is a file nobody knows to look for. Clearing it once brings those
// devices back.
//
// Once, not every start: HDMI remains a setting an operator can turn off, and
// re-enabling it on every restart would quietly make that setting a lie. The
// marker is what keeps this a migration rather than a policy.
func ClearHDMIDisabledOnce() {
	if _, err := os.Stat(hdmiClearedMarker); err == nil {
		return // already done on this device
	}

	if err := os.Remove(HDMIDisableFile); err == nil {
		log.Infof("hdmi: cleared a stale disable flag — HDMI is on this boot")
	} else if !os.IsNotExist(err) {
		// Leave the marker unwritten so the next start tries again: a device we
		// failed to heal must not be recorded as healed.
		log.Warnf("hdmi: could not remove %s: %s", HDMIDisableFile, err)
		return
	}

	if err := os.WriteFile(hdmiClearedMarker, []byte("1\n"), 0o644); err != nil {
		log.Warnf("hdmi: could not write %s: %s", hdmiClearedMarker, err)
	}
}
