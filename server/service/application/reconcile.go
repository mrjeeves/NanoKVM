package application

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"NanoKVM-Server/utils"
)

// ReconcileDaemon converges a device onto the myownmesh daemon this server
// release pins, healing the one gap the over-the-air path can't cover itself:
// a device updated FROM an older server (whose updater swapped only the server
// + web and ignored the daemon) boots the new server still running the previous
// daemon. This server can't have installed its own daemon during that update —
// the old code did the installing — so it does it now, on startup.
//
// It runs at most once per server version (a marker under stateDir records the
// version last reconciled), so an ordinary boot does no work and never touches
// the network. When the marker is stale it fetches our release bundle for this
// exact version and swaps in the pinned daemon only if the installed binary
// differs, then restarts the daemon (not the server — the running bridge simply
// reconnects to it). Best-effort with backoff so a device whose network or
// clock isn't up at boot still converges without waiting for a reboot; if it
// never succeeds, the next boot retries.
//
// Meant to be called in its own goroutine — it sleeps and does network I/O, and
// must never take down the server, so it recovers from panics and only logs.
func ReconcileDaemon(version, daemonBin, stateDir string) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("daemon reconcile panicked: %v", r)
		}
	}()

	// A dev build (no release tag) has no published bundle to reconcile against,
	// and without the paths there's nothing to do.
	if version == "" || daemonBin == "" || stateDir == "" {
		return
	}

	marker := filepath.Join(stateDir, daemonSyncMarker)
	if synced, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(synced)) == version {
		return // this server version already ensured its daemon
	}

	// Back off across attempts: the network (and, on a no-RTC box, a real clock
	// for GitHub's TLS) may not be up the instant the server starts. Bounded —
	// if none succeed we leave the marker unwritten and retry on the next boot.
	for attempt, delay := range []time.Duration{
		30 * time.Second,
		60 * time.Second,
		2 * time.Minute,
		5 * time.Minute,
		5 * time.Minute,
	} {
		time.Sleep(delay)

		changed, err := reconcileDaemon(version, daemonBin)
		if err != nil {
			log.Warnf("daemon reconcile attempt %d for %s failed: %v", attempt+1, version, err)
			continue
		}

		if changed {
			log.Infof("daemon reconcile: installed the %s daemon; restarting it", version)
			_ = exec.Command("sh", "-c", daemonRestartCmd).Run()
		} else {
			log.Infof("daemon reconcile: daemon already current for %s", version)
		}
		// Record success (whether or not the daemon changed) so we don't re-run
		// — or re-download — on every subsequent boot of this version.
		if err := os.MkdirAll(stateDir, 0o755); err == nil {
			_ = os.WriteFile(marker, []byte(version+"\n"), 0o644)
		}
		return
	}
	log.Warnf("daemon reconcile for %s did not complete; will retry on next boot", version)
}

// reconcileDaemon downloads our release bundle for `version`, and if it carries
// a myownmesh daemon that differs from the installed binary, swaps it in. It
// reports whether the daemon changed. Serialized with the update endpoint via
// the same lock so the two never fight over the cache dir.
func reconcileDaemon(version, daemonBin string) (bool, error) {
	if !acquireUpdateLock() {
		// An update is running; it will install the daemon itself. Retry later.
		return false, fmt.Errorf("an update is in progress")
	}
	defer releaseUpdateLock()

	_ = os.RemoveAll(CacheDir)
	if err := os.MkdirAll(CacheDir, 0o755); err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(CacheDir) }()

	// Fetch the device bundle for THIS server's exact version (a `vX.Y.Z` tag),
	// not "latest": the daemon we want is the one this release pins.
	url := bundleURL("v"+version, bundleAsset)
	bundlePath := filepath.Join(CacheDir, bundleAsset)
	if err := downloadWithRetry(url, bundlePath); err != nil {
		return false, fmt.Errorf("download bundle: %w", err)
	}
	shaPath := bundlePath + ".sha256"
	if err := downloadWithRetry(url+".sha256", shaPath); err != nil {
		return false, fmt.Errorf("download checksum: %w", err)
	}
	if err := verifySha256(bundlePath, shaPath); err != nil {
		return false, fmt.Errorf("checksum: %w", err)
	}

	extractDir := filepath.Join(CacheDir, "bundle")
	if _, err := utils.UnTarGz(bundlePath, extractDir); err != nil {
		return false, fmt.Errorf("extract: %w", err)
	}
	return installDaemonFromBundle(extractDir, daemonBin)
}

// installDaemonFromBundle swaps the bundle's myownmesh into daemonDst when it
// differs from the installed binary, reporting whether it changed. A bundle
// that carries no daemon is a no-op.
func installDaemonFromBundle(bundleDir, daemonDst string) (bool, error) {
	daemonSrc := filepath.Join(bundleDir, "myownmesh")
	if !isFile(daemonSrc) {
		return false, nil
	}
	differs, err := filesDiffer(daemonSrc, daemonDst)
	if err != nil {
		return false, fmt.Errorf("compare daemon: %w", err)
	}
	if !differs {
		return false, nil
	}
	if err := atomicSwapFile(daemonSrc, daemonDst); err != nil {
		return false, fmt.Errorf("install daemon: %w", err)
	}
	return true, nil
}

// atomicSwapFile replaces dst with a copy of src (mode 0755): stage dst.new,
// move any current dst aside to dst.old, rename the stage in, then drop the
// backup. On failure it restores from the backup. Renaming over the live daemon
// keeps the old inode for the running process, so this is safe and never hits
// ETXTBSY the way an in-place copy would.
func atomicSwapFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	stage := dst + ".new"
	backup := dst + ".old"
	if err := copyFileMode(src, stage, 0o755); err != nil {
		return err
	}
	_ = os.Remove(backup)
	if isFile(dst) {
		if err := os.Rename(dst, backup); err != nil {
			_ = os.Remove(stage)
			return err
		}
	}
	if err := os.Rename(stage, dst); err != nil {
		_ = os.Remove(stage)
		restore(backup, dst)
		return err
	}
	_ = os.Remove(backup)
	return nil
}
