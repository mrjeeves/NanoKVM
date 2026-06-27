package mesh

import (
	"bufio"
	"math/rand"
	"os"
	"strconv"
	"strings"

	"NanoKVM-Server/config"
)

// deviceInfo is a CGO-free snapshot of the host's identity + hardware thumbnail.
// We deliberately gather it here (os/proc reads) rather than importing
// server/service/vm, which pulls in CGO/libkvm via config hardware + common.
type deviceInfo struct {
	hostname string
	summary  InventorySummary
}

// gatherDeviceInfo reads hostname and a hardware thumbnail from /proc and /etc.
// Everything is best-effort: a missing file just leaves a field empty.
func gatherDeviceInfo() deviceInfo {
	return deviceInfo{
		hostname: readHostname(),
		summary: InventorySummary{
			OS:          "linux",
			CPU:         readCPUModel(),
			RAMBytes:    readTotalRAMBytes(),
			DeviceCount: 1, // the KVM appliance itself
		},
	}
}

func readHostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	if raw, err := os.ReadFile("/etc/hostname"); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return ""
}

func readCPUModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		// x86 uses "model name", many ARM/RISC-V kernels use "Hardware",
		// "uarch", or "isa". Take the first informative one.
		for _, key := range []string{"model name", "Hardware", "uarch", "cpu model", "isa"} {
			if strings.HasPrefix(line, key) {
				if i := strings.Index(line, ":"); i >= 0 {
					if v := strings.TrimSpace(line[i+1:]); v != "" {
						return v
					}
				}
			}
		}
	}
	return ""
}

func readTotalRAMBytes() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					return kb * 1024 // MemTotal is in kB
				}
			}
		}
	}
	return 0
}

// webPort returns the port the KVM web UI listens on, per config.
func webPort(conf *config.Config) uint16 {
	if conf.Proto == "https" {
		return uint16(conf.Port.Https)
	}
	return uint16(conf.Port.Http)
}

// webScheme returns the URL scheme the web UI is reached with.
func webScheme(conf *config.Config) string {
	if conf.Proto == "https" {
		return "https"
	}
	return "http"
}

// siteID is the SiteAdvert id (and KvmAdvert.web) for our web UI — "tcp:<port>",
// mirroring the scan's ListeningService.id convention.
func siteID(port uint16) string {
	return "tcp:" + strconv.Itoa(int(port))
}

// buildProfile assembles the presence NodeProfile from device info, config, and
// the current persisted state. nodeID is our daemon device id; version is the
// NanoKVM application version; boot is the random per-run boot id.
func buildProfile(nodeID string, conf *config.Config, dev deviceInfo, st *State, version string, boot uint64) NodeProfile {
	port := webPort(conf)
	id := siteID(port)
	snap := st.snapshot()

	label := dev.hostname
	if label == "" {
		label = nodeID
	}

	var owner *string
	if snap.Owner != "" {
		o := snap.Owner
		owner = &o
	}

	var attached *string
	if snap.AttachedTo != "" {
		a := snap.AttachedTo
		attached = &a
	}

	return NodeProfile{
		Protocol:     ProtocolVersion,
		Node:         nodeID,
		Label:        label,
		Hostname:     dev.hostname,
		Summary:      dev.summary,
		Capabilities: []Capability{}, // none in v1 — the tunneled web UI carries everything
		Owner:        owner,
		Claimable:    snap.Claimable,
		Boot:         boot,
		Features:     []string{FeatureKVM, FeatureSites},
		Sites: []SiteAdvert{{
			ID:       id,
			Label:    "KVM Web UI",
			Port:     port,
			Scheme:   webScheme(conf),
			Loopback: false,
		}},
		Version:    version,
		FleetName:  snap.FleetName,
		FleetOwner: snap.FleetName, // a fleet is named for its owner; track it
		Kvm: &KvmAdvert{
			AttachedTo: attached,
			Web:        id,
		},
	}
}

// newBootID mints a random per-run boot id (never 0, which means "older peer").
func newBootID() uint64 {
	b := rand.Uint64()
	if b == 0 {
		b = 1
	}
	return b
}

// readFileTrim reads a file and trims surrounding whitespace/newlines.
func readFileTrim(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
