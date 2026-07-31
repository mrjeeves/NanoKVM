package application

import (
	"os"
	"path/filepath"
	"testing"
)

// bundleWithDaemon builds a temp "extracted bundle" dir carrying a myownmesh
// binary with the given content.
func bundleWithDaemon(t *testing.T, root, content string) string {
	t.Helper()
	dir := filepath.Join(root, "bundle")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "myownmesh"), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func installedDaemon(t *testing.T, root, content string) string {
	t.Helper()
	p := filepath.Join(root, "system", "bin", "myownmesh")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallDaemonFromBundleReplacesChanged(t *testing.T) {
	root := t.TempDir()
	bundleDir := bundleWithDaemon(t, root, "new-daemon")
	dst := installedDaemon(t, root, "old-daemon")

	changed, err := installDaemonFromBundle(bundleDir, dst)
	if err != nil {
		t.Fatalf("installDaemonFromBundle: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true for a differing daemon")
	}
	if got := read(t, dst); got != "new-daemon" {
		t.Errorf("daemon = %q, want new-daemon", got)
	}
	for _, leftover := range []string{"myownmesh.new", "myownmesh.old"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(dst), leftover)); !os.IsNotExist(err) {
			t.Errorf("leftover %s not cleaned up", leftover)
		}
	}
}

func TestInstallDaemonFromBundleFreshInstall(t *testing.T) {
	root := t.TempDir()
	bundleDir := bundleWithDaemon(t, root, "the-daemon")
	dst := filepath.Join(root, "system", "bin", "myownmesh") // nothing installed

	changed, err := installDaemonFromBundle(bundleDir, dst)
	if err != nil {
		t.Fatalf("installDaemonFromBundle: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true when no daemon was installed")
	}
	if got := read(t, dst); got != "the-daemon" {
		t.Errorf("daemon = %q, want the-daemon", got)
	}
}

func TestInstallDaemonFromBundleUnchanged(t *testing.T) {
	root := t.TempDir()
	bundleDir := bundleWithDaemon(t, root, "same-daemon")
	dst := installedDaemon(t, root, "same-daemon")

	changed, err := installDaemonFromBundle(bundleDir, dst)
	if err != nil {
		t.Fatalf("installDaemonFromBundle: %v", err)
	}
	if changed {
		t.Error("changed = true, want false for a byte-identical daemon")
	}
	if _, err := os.Stat(dst + ".old"); !os.IsNotExist(err) {
		t.Errorf("an unchanged daemon should not create a backup")
	}
}

func TestInstallDaemonFromBundleNoDaemonInBundle(t *testing.T) {
	root := t.TempDir()
	bundleDir := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := installedDaemon(t, root, "keep-me")

	changed, err := installDaemonFromBundle(bundleDir, dst)
	if err != nil {
		t.Fatalf("installDaemonFromBundle: %v", err)
	}
	if changed {
		t.Error("changed = true, want false when the bundle has no daemon")
	}
	if got := read(t, dst); got != "keep-me" {
		t.Errorf("daemon = %q, want keep-me (untouched)", got)
	}
}

func TestAtomicSwapFile(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.WriteFile(src, []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Over an existing dst.
	dst := filepath.Join(root, "nested", "dst")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := atomicSwapFile(src, dst); err != nil {
		t.Fatalf("atomicSwapFile over existing: %v", err)
	}
	if got := read(t, dst); got != "payload" {
		t.Errorf("dst = %q, want payload", got)
	}
	if _, err := os.Stat(dst + ".old"); !os.IsNotExist(err) {
		t.Errorf("backup not cleaned up")
	}
	if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
		t.Errorf("stage not cleaned up")
	}

	// Onto a path with no existing dst (dir auto-created).
	fresh := filepath.Join(root, "brand", "new", "dst")
	if err := atomicSwapFile(src, fresh); err != nil {
		t.Fatalf("atomicSwapFile fresh: %v", err)
	}
	if got := read(t, fresh); got != "payload" {
		t.Errorf("fresh dst = %q, want payload", got)
	}
}

// The gap this whole file exists for, widened: the code that performs an update
// is the code ALREADY ON THE DEVICE. A device updating from a server that
// predates init.d-in-the-bundle is updated by that server's updater, which
// copies NanoKVM-Server, web and myownmesh and silently ignores an init.d/ it
// has never heard of. So the new server boots with its own boot scripts
// missing — S03usbdev, S32usbdhcp — and nothing on-device fixes that until the
// NEXT release. The startup reconcile closes it in one hop.
func TestInstallReleaseFromBundleDeliversScriptsAndDaemon(t *testing.T) {
	root := t.TempDir()
	bundle := bundleWithDaemon(t, root, "new-daemon")
	scriptDir := filepath.Join(bundle, "init.d")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"S03usbdev", "S32usbdhcp"} {
		if err := os.WriteFile(filepath.Join(scriptDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	daemon := installedDaemon(t, root, "old-daemon")

	target := t.TempDir()
	orig := initScriptDirForTest
	initScriptDirForTest = target
	defer func() { initScriptDirForTest = orig }()

	daemonChanged, scripts, err := installReleaseFromBundle(bundle, daemon)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !daemonChanged {
		t.Error("daemon differed but was not replaced")
	}
	if scripts != 2 {
		t.Errorf("installed %d scripts, want 2", scripts)
	}
	for _, name := range []string{"S03usbdev", "S32usbdhcp"} {
		fi, err := os.Stat(filepath.Join(target, name))
		if err != nil {
			t.Errorf("%s not installed: %v", name, err)
			continue
		}
		// run-parts skips a file it can't execute, so a non-executable script
		// is indistinguishable from one that never shipped.
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s installed mode %v is not executable", name, fi.Mode().Perm())
		}
	}
}

// A bundle whose daemon already matches must still deliver a changed script.
// The two are independent payloads; gating one on the other is how a device
// ends up current in one half and stale in the other.
func TestInstallReleaseFromBundleShipsScriptsWithUnchangedDaemon(t *testing.T) {
	root := t.TempDir()
	bundle := bundleWithDaemon(t, root, "same-daemon")
	scriptDir := filepath.Join(bundle, "init.d")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scriptDir, "S03usbdev"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	daemon := installedDaemon(t, root, "same-daemon")

	target := t.TempDir()
	orig := initScriptDirForTest
	initScriptDirForTest = target
	defer func() { initScriptDirForTest = orig }()

	daemonChanged, scripts, err := installReleaseFromBundle(bundle, daemon)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if daemonChanged {
		t.Error("identical daemon was replaced")
	}
	if scripts != 1 {
		t.Errorf("installed %d scripts, want 1", scripts)
	}
}
