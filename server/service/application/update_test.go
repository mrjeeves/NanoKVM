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
	changed, err := installBundle(bundle, appDir)
	if err != nil {
		t.Fatalf("installBundle: %v", err)
	}
	if changed {
		t.Errorf("daemonChanged = true for a bundle carrying no daemon")
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
	if _, err := installBundle(bundle, appDir); err != nil {
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

	if _, err := installBundle(badBundle, appDir); err == nil {
		t.Fatal("installBundle accepted a bundle with no web/")
	}
	// The running server must be untouched.
	if got := read(t, filepath.Join(appDir, "server", "NanoKVM-Server")); got != "current-server" {
		t.Errorf("server was modified on a rejected bundle: %q", got)
	}
}

// seedDaemon writes an installed daemon binary at appDir/system/bin/myownmesh.
func seedDaemon(t *testing.T, appDir, content string) string {
	t.Helper()
	p := filepath.Join(appDir, "system", "bin", "myownmesh")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// addDaemon writes a myownmesh binary into an already-built extracted bundle.
func addDaemon(t *testing.T, bundleDir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bundleDir, "myownmesh"), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInstallBundleReplacesChangedDaemon(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "kvmapp")
	daemonPath := seedDaemon(t, appDir, "old-daemon")

	bundle := makeBundle(t, root, "srv", "web")
	addDaemon(t, bundle, "new-daemon")

	changed, err := installBundle(bundle, appDir)
	if err != nil {
		t.Fatalf("installBundle: %v", err)
	}
	if !changed {
		t.Fatal("daemonChanged = false, want true for a changed daemon")
	}
	if got := read(t, daemonPath); got != "new-daemon" {
		t.Errorf("daemon = %q, want new-daemon", got)
	}
	// No leftover staging/backup beside the daemon.
	for _, leftover := range []string{"myownmesh.new", "myownmesh.old"} {
		if _, err := os.Stat(filepath.Join(appDir, "system", "bin", leftover)); !os.IsNotExist(err) {
			t.Errorf("leftover %s not cleaned up", leftover)
		}
	}
}

func TestInstallBundleInstallsDaemonOnFreshDevice(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "kvmapp") // no daemon seeded

	bundle := makeBundle(t, root, "srv", "web")
	addDaemon(t, bundle, "the-daemon")

	changed, err := installBundle(bundle, appDir)
	if err != nil {
		t.Fatalf("installBundle: %v", err)
	}
	if !changed {
		t.Fatal("daemonChanged = false, want true when no daemon was installed yet")
	}
	if got := read(t, filepath.Join(appDir, "system", "bin", "myownmesh")); got != "the-daemon" {
		t.Errorf("daemon = %q, want the-daemon", got)
	}
}

func TestInstallBundleLeavesUnchangedDaemon(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "kvmapp")
	daemonPath := seedDaemon(t, appDir, "same-daemon")

	bundle := makeBundle(t, root, "srv", "web")
	addDaemon(t, bundle, "same-daemon") // byte-identical to the installed one

	changed, err := installBundle(bundle, appDir)
	if err != nil {
		t.Fatalf("installBundle: %v", err)
	}
	if changed {
		t.Error("daemonChanged = true, want false when the daemon is byte-identical")
	}
	if got := read(t, daemonPath); got != "same-daemon" {
		t.Errorf("daemon = %q, want same-daemon", got)
	}
	if _, err := os.Stat(daemonPath + ".old"); !os.IsNotExist(err) {
		t.Errorf("an unchanged daemon should not create a backup")
	}
}

// An OTA has to be able to deliver a boot script. The bundle carried only the
// server, web and daemon, so a new script the server depends on — S32usbdhcp,
// which is what surfaced this — reached a device only by a full flash or
// `just deploy`. That fails in the worst way: the server arrives expecting
// something the device hasn't got, and nothing says so.
func TestInstallInitScriptsPlacesBundledScripts(t *testing.T) {
	bundle := t.TempDir()
	dir := filepath.Join(bundle, "init.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "S32usbdhcp"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Redirect the install target; the real one is /etc/init.d.
	target := t.TempDir()
	orig := initScriptDirForTest
	initScriptDirForTest = target
	defer func() { initScriptDirForTest = orig }()

	installInitScripts(bundle)

	got := filepath.Join(target, "S32usbdhcp")
	fi, err := os.Stat(got)
	if err != nil {
		t.Fatalf("script not installed: %v", err)
	}
	// Must land executable — rcS runs it via run-parts, which skips a file it
	// can't execute, so a non-executable script is the same as an absent one.
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed mode %v is not executable", fi.Mode().Perm())
	}
}

// A bundle from an older release has no init.d/. That must be a quiet no-op,
// never an error that fails an update which has already swapped in a working
// server and web.
func TestInstallInitScriptsIgnoresBundleWithout(t *testing.T) {
	installInitScripts(t.TempDir()) // must not panic
}

// RNDIS cannot work on any current host — Windows 11 ships no in-box driver and
// macOS never had one — so a device carrying that flag has a USB link that is up
// at every layer except the one that matters. An update migrates it rather than
// preserving it.
func TestMigrateUsbNetworkFlagSwitchesRndisToNcm(t *testing.T) {
	dir := t.TempDir()
	ncm, rndis := filepath.Join(dir, "usb.ncm"), filepath.Join(dir, "usb.rndis0")
	orig := [2]string{usbFlagNcm, usbFlagRndis}
	usbFlagNcm, usbFlagRndis = ncm, rndis
	defer func() { usbFlagNcm, usbFlagRndis = orig[0], orig[1] }()

	if err := os.WriteFile(rndis, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	migrateUsbNetworkFlag()

	if _, err := os.Stat(ncm); err != nil {
		t.Fatalf("NCM flag not written: %v", err)
	}
	if _, err := os.Stat(rndis); err == nil {
		t.Fatal("RNDIS flag still present — S03usbdev would still see a choice to make")
	}
}

// Neither flag means the operator turned the virtual network OFF. That is a
// real choice, and an update must not switch a new USB interface on underneath
// a running deployment.
func TestMigrateUsbNetworkFlagLeavesDisabledAlone(t *testing.T) {
	dir := t.TempDir()
	ncm, rndis := filepath.Join(dir, "usb.ncm"), filepath.Join(dir, "usb.rndis0")
	orig := [2]string{usbFlagNcm, usbFlagRndis}
	usbFlagNcm, usbFlagRndis = ncm, rndis
	defer func() { usbFlagNcm, usbFlagRndis = orig[0], orig[1] }()

	migrateUsbNetworkFlag()

	if _, err := os.Stat(ncm); err == nil {
		t.Fatal("an update enabled the USB network on a device that had it off")
	}
}

// Both present: S03usbdev prefers NCM, so that is already what boots. Drop the
// stale RNDIS flag so the two can't disagree about what the device is.
func TestMigrateUsbNetworkFlagClearsStaleRndis(t *testing.T) {
	dir := t.TempDir()
	ncm, rndis := filepath.Join(dir, "usb.ncm"), filepath.Join(dir, "usb.rndis0")
	orig := [2]string{usbFlagNcm, usbFlagRndis}
	usbFlagNcm, usbFlagRndis = ncm, rndis
	defer func() { usbFlagNcm, usbFlagRndis = orig[0], orig[1] }()

	for _, p := range []string{ncm, rndis} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	migrateUsbNetworkFlag()

	if _, err := os.Stat(ncm); err != nil {
		t.Fatalf("NCM flag removed: %v", err)
	}
	if _, err := os.Stat(rndis); err == nil {
		t.Fatal("stale RNDIS flag kept alongside NCM")
	}
}
