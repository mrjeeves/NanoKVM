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
