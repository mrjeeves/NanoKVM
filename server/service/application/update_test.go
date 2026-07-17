package application

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestBundleURL(t *testing.T) {
	cases := []struct {
		version string
		want    string
	}{
		{"", releaseBaseURL + "/latest/download/" + bundleAsset},
		{"latest", releaseBaseURL + "/latest/download/" + bundleAsset},
		{"v0.1.0", releaseBaseURL + "/download/v0.1.0/" + bundleAsset},
	}
	for _, c := range cases {
		if got := bundleURL(c.version, bundleAsset); got != c.want {
			t.Errorf("bundleURL(%q) = %q, want %q", c.version, got, c.want)
		}
	}
}

func TestTagPattern(t *testing.T) {
	good := []string{"v0.1.0", "v1.2.3", "v0.3.1-rc1"}
	bad := []string{"0.1.0", "v0.1.0/../etc", "http://evil", "v0.1.0 x", "latest"}
	for _, v := range good {
		if !tagPattern.MatchString(v) {
			t.Errorf("tagPattern rejected valid tag %q", v)
		}
	}
	for _, v := range bad {
		if tagPattern.MatchString(v) {
			t.Errorf("tagPattern accepted invalid tag %q", v)
		}
	}
}

func writeSha256File(t *testing.T, dir, bundleName string, data []byte) (bundlePath, shaPath string) {
	t.Helper()
	bundlePath = filepath.Join(dir, bundleName)
	if err := os.WriteFile(bundlePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	shaPath = bundlePath + ".sha256"
	line := hex.EncodeToString(sum[:]) + "  " + bundleName + "\n"
	if err := os.WriteFile(shaPath, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return bundlePath, shaPath
}

func TestVerifySha256(t *testing.T) {
	dir := t.TempDir()
	bundlePath, shaPath := writeSha256File(t, dir, "bundle.tar.gz", []byte("firmware payload"))

	if err := verifySha256(bundlePath, shaPath); err != nil {
		t.Fatalf("verifySha256 rejected a matching checksum: %v", err)
	}

	// Tamper with the payload → mismatch must be caught.
	if err := os.WriteFile(bundlePath, []byte("tampered payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifySha256(bundlePath, shaPath); err == nil {
		t.Fatal("verifySha256 accepted a tampered payload")
	}
}

func TestExpectedSha256(t *testing.T) {
	dir := t.TempDir()
	hexSum := ""
	for i := 0; i < 64; i++ {
		hexSum += "a"
	}
	good := filepath.Join(dir, "good.sha256")
	if err := os.WriteFile(good, []byte(hexSum+"  bundle.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := expectedSha256(good)
	if err != nil || got != hexSum {
		t.Fatalf("expectedSha256 = %q, %v; want %q", got, err, hexSum)
	}

	bad := filepath.Join(dir, "bad.sha256")
	if err := os.WriteFile(bad, []byte("not-a-hash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := expectedSha256(bad); err == nil {
		t.Fatal("expectedSha256 accepted a malformed file")
	}
}

// makeBundle builds a fake extracted bundle dir with the two artifacts the
// installer places: the server binary and a web/ tree.
func makeBundle(t *testing.T, root, serverContent, webContent string) string {
	t.Helper()
	dir := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(dir, "web", "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "NanoKVM-Server"), []byte(serverContent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "web", "index.html"), []byte(webContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "web", "assets", "app.js"), []byte(webContent), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallBundleReplacesServerAndWeb(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "kvmapp")

	// Seed an existing install with an old server + an old web file that the
	// new web tree does NOT carry — it must be gone after the swap.
	if err := os.MkdirAll(filepath.Join(appDir, "server", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "server", "NanoKVM-Server"), []byte("old-server"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "server", "web", "stale.html"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	bundle := makeBundle(t, root, "new-server", "new-web")
	if err := installBundle(bundle, appDir); err != nil {
		t.Fatalf("installBundle: %v", err)
	}

	if got := read(t, filepath.Join(appDir, "server", "NanoKVM-Server")); got != "new-server" {
		t.Errorf("server = %q, want new-server", got)
	}
	if got := read(t, filepath.Join(appDir, "server", "web", "index.html")); got != "new-web" {
		t.Errorf("web/index.html = %q, want new-web", got)
	}
	if got := read(t, filepath.Join(appDir, "server", "web", "assets", "app.js")); got != "new-web" {
		t.Errorf("web/assets/app.js = %q, want new-web", got)
	}
	// The stale file the new tree doesn't carry is gone (web was replaced, not merged).
	if _, err := os.Stat(filepath.Join(appDir, "server", "web", "stale.html")); !os.IsNotExist(err) {
		t.Errorf("stale web file survived the swap")
	}
	// No leftover staging/backup dirs.
	for _, leftover := range []string{"NanoKVM-Server.new", "NanoKVM-Server.old", "web.new", "web.old"} {
		if _, err := os.Stat(filepath.Join(appDir, "server", leftover)); !os.IsNotExist(err) {
			t.Errorf("leftover %s not cleaned up", leftover)
		}
	}
}

func TestInstallBundleFreshInstall(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "kvmapp") // nothing seeded

	bundle := makeBundle(t, root, "srv", "web")
	if err := installBundle(bundle, appDir); err != nil {
		t.Fatalf("installBundle on a fresh dir: %v", err)
	}
	if got := read(t, filepath.Join(appDir, "server", "NanoKVM-Server")); got != "srv" {
		t.Errorf("server = %q, want srv", got)
	}
	if got := read(t, filepath.Join(appDir, "server", "web", "index.html")); got != "web" {
		t.Errorf("web = %q, want web", got)
	}
}

func TestInstallBundleRejectsIncompleteBundleWithoutTouchingCurrent(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "kvmapp")
	if err := os.MkdirAll(filepath.Join(appDir, "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "server", "NanoKVM-Server"), []byte("current-server"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A bundle with the server but no web/ must be refused before any swap.
	badBundle := filepath.Join(root, "bad")
	if err := os.MkdirAll(badBundle, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badBundle, "NanoKVM-Server"), []byte("new-server"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := installBundle(badBundle, appDir); err == nil {
		t.Fatal("installBundle accepted a bundle with no web/")
	}
	// The running server must be untouched.
	if got := read(t, filepath.Join(appDir, "server", "NanoKVM-Server")); got != "current-server" {
		t.Errorf("server was modified on a rejected bundle: %q", got)
	}
}
