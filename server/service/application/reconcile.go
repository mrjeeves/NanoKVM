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

// ReconcileRelease converges a device onto the rest of the payload this server
// release ships — the pinned myownmesh daemon and the boot scripts — healing the
// one gap the over-the-air path can't cover itself.
//
// The code that performs an update is the code ALREADY ON THE DEVICE. A device
// updating from an older server is updated by that older server's updater, which
// installs only the parts it knows about: the server and web, and (before the
// daemon shipped) not the daemon, and (before init.d shipped) not the boot
// scripts. So the new server boots with pieces of its own release missing, and
// nothing on-device fixes that. It can't have installed them during the very
// update that installed it — so it does it now, on startup.
//
// Each new part of the bundle re-opens this hole for one release, which is why
// this exists rather than being folded into the updater: the updater only ever
// helps the NEXT upgrade, never the one that delivers it.
//
// It runs at most once per server version (a marker under stateDir records the
// version last reconciled), so an ordinary boot does no work and never touches
// the network. When the marker is stale it fetches our release bundle for this
// exact version and installs what's missing: the pinned daemon if the installed
// binary differs, and any boot script that differs. A changed daemon is
// restarted (not the server — the running bridge simply reconnects to it).
// Changed boot scripts are NOT run: their effects belong to a boot, and one of
// them composes the USB gadget, which is how a KVM loses its keyboard
// mid-session. They take effect at the device's next restart.
//
// Best-effort with backoff so a device whose network or clock isn't up at boot
// still converges without waiting for a reboot; if it never succeeds, the next
// boot retries.
//
// Meant to be called in its own goroutine — it sleeps and does network I/O, and
// must never take down the server, so it recovers from panics and only logs.
func ReconcileRelease(version, daemonBin, stateDir string) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("release reconcile panicked: %v", r)
		}
	}()

	// A dev build (no release tag) has no published bundle to reconcile against,
	// and without the paths there's nothing to do.
	if version == "" || daemonBin == "" || stateDir == "" {
		return
	}

	marker := filepath.Join(stateDir, releaseSyncMarker)
	if synced, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(synced)) == version {
		return // this server version already reconciled its payload
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

		daemonChanged, scripts, err := reconcileRelease(version, daemonBin)
		if err != nil {
			log.Warnf("release reconcile attempt %d for %s failed: %v", attempt+1, version, err)
			continue
		}

		if daemonChanged {
			log.Infof("release reconcile: installed the %s daemon; restarting it", version)
			_ = exec.Command("sh", "-c", daemonRestartCmd).Run()
		}
		if scripts > 0 {
			log.Infof("release reconcile: installed %d boot script(s) for %s; they take effect on the next restart",
				scripts, version)
		}
		if !daemonChanged && scripts == 0 {
			log.Infof("release reconcile: already current for %s", version)
		}
		// Record success (whether or not anything changed) so we don't re-run
		// — or re-download — on every subsequent boot of this version.
		if err := os.MkdirAll(stateDir, 0o755); err == nil {
			_ = os.WriteFile(marker, []byte(version+"\n"), 0o644)
		}
		return
	}
	log.Warnf("release reconcile for %s did not complete; will retry on next boot", version)
}

// reconcileRelease downloads our release bundle for `version` and installs the
// parts of it that differ from what's on the device: the myownmesh daemon, and
// the boot scripts. It reports whether the daemon changed and how many scripts
// were written. Serialized with the update endpoint via the same lock so the two
// never fight over the cache dir.
func reconcileRelease(version, daemonBin string) (bool, int, error) {
	if !acquireUpdateLock() {
		// An update is running; it installs the same payload itself. Retry later.
		return false, 0, fmt.Errorf("an update is in progress")
	}
	defer releaseUpdateLock()

	_ = os.RemoveAll(CacheDir)
	if err := os.MkdirAll(CacheDir, 0o755); err != nil {
		return false, 0, err
	}
	defer func() { _ = os.RemoveAll(CacheDir) }()

	// Fetch the device bundle for THIS server's exact version (a `vX.Y.Z` tag),
	// not "latest": the daemon we want is the one this release pins.
	url := bundleURL("v"+version, bundleAsset)
	bundlePath := filepath.Join(CacheDir, bundleAsset)
	if err := downloadWithRetry(url, bundlePath); err != nil {
		return false, 0, fmt.Errorf("download bundle: %w", err)
	}
	shaPath := bundlePath + ".sha256"
	if err := downloadWithRetry(url+".sha256", shaPath); err != nil {
		return false, 0, fmt.Errorf("download checksum: %w", err)
	}
	if err := verifySha256(bundlePath, shaPath); err != nil {
		return false, 0, fmt.Errorf("checksum: %w", err)
	}

	extractDir := filepath.Join(CacheDir, "bundle")
	if _, err := utils.UnTarGz(bundlePath, extractDir); err != nil {
		return false, 0, fmt.Errorf("extract: %w", err)
	}

	return installReleaseFromBundle(extractDir, daemonBin)
}

// installReleaseFromBundle installs the parts of an extracted bundle that a
// device updated by an older server never received: the boot scripts and the
// pinned daemon. Reports whether the daemon changed and how many scripts were
// written.
//
// Scripts first, and their result is returned even when the daemon step fails:
// they're independent payloads, a bundle whose daemon already matches must
// still be able to deliver a script, and having placed one is worth recording
// either way.
func installReleaseFromBundle(bundleDir, daemonBin string) (bool, int, error) {
	scripts := installInitScripts(bundleDir)
	daemonChanged, err := installDaemonFromBundle(bundleDir, daemonBin)
	if err != nil {
		return false, scripts, err
	}
	return daemonChanged, scripts, nil
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
