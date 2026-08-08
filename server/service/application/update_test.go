package application

import (
	"bytes"
	"compress/gzip"
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

	if n := installInitScripts(bundle); n != 1 {
		t.Fatalf("installed %d scripts, want 1", n)
	}

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

	// Re-running must be a no-op. The startup reconcile calls this on a bundle
	// the device may already match, and rewriting identical files onto flash —
	// while logging "installed" — is both wear and a lie.
	if n := installInitScripts(bundle); n != 0 {
		t.Fatalf("re-install wrote %d scripts, want 0", n)
	}

	// A script that really did change is written, though.
	if err := os.WriteFile(filepath.Join(dir, "S32usbdhcp"), []byte("#!/bin/sh\nexit 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := installInitScripts(bundle); n != 1 {
		t.Fatalf("changed script: installed %d, want 1", n)
	}
	body, err := os.ReadFile(got)
	if err != nil || string(body) != "#!/bin/sh\nexit 1\n" {
		t.Fatalf("changed script not written through: %q (%v)", body, err)
	}
}

// A bundle from an older release has no init.d/. That must be a quiet no-op,
// never an error that fails an update which has already swapped in a working
// server and web.
func TestInstallInitScriptsIgnoresBundleWithout(t *testing.T) {
	if n := installInitScripts(t.TempDir()); n != 0 { // must not panic
		t.Fatalf("installed %d scripts from an empty bundle", n)
	}
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

	MigrateUsbNetworkFlag()

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

	MigrateUsbNetworkFlag()

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

	MigrateUsbNetworkFlag()

	if _, err := os.Stat(ncm); err != nil {
		t.Fatalf("NCM flag removed: %v", err)
	}
	if _, err := os.Stat(rndis); err == nil {
		t.Fatal("stale RNDIS flag kept alongside NCM")
	}
}

// gzipFile writes `body` gzipped to `path` — a stand-in for the bundle's
// prebuilt drive image.
func gzipFile(t *testing.T, path string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fatImage returns bytes that look like a formatted FAT volume: the 0x55AA boot
// signature at offset 510 is what every mkfs.vfat volume carries.
func fatImage() []byte {
	img := make([]byte, 1024)
	img[510], img[511] = 0x55, 0xAA
	return img
}

// The bug this replaced: S03usbdev built the image itself and treated "a file
// of the right size exists" as "it's formatted". A boot interrupted mid-format
// left a sized-but-empty file, and every boot after exported it — so Windows
// asked the customer to format their KVM, forever. The image is built in CI
// now, and a device carrying the broken one must be repaired.
func TestInstallUsbDiskReplacesAnUnformattedImage(t *testing.T) {
	bundle := t.TempDir()
	gzipFile(t, filepath.Join(bundle, "usbdisk.img.gz"), fatImage())

	dst := filepath.Join(t.TempDir(), "usbdisk.img")
	// Exactly the broken state: right size, no filesystem.
	if err := os.WriteFile(dst, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := usbDiskImageForTest
	usbDiskImageForTest = dst
	defer func() { usbDiskImageForTest = orig }()

	if !installUsbDisk(bundle) {
		t.Fatal("an unformatted image was not replaced")
	}
	if !looksFormatted(dst) {
		t.Error("the replacement isn't a filesystem either")
	}
	// No stage file left behind.
	if isFile(dst + ".new") {
		t.Error("the staging file survived")
	}
}

func TestInstallUsbDiskCreatesAMissingImage(t *testing.T) {
	bundle := t.TempDir()
	gzipFile(t, filepath.Join(bundle, "usbdisk.img.gz"), fatImage())

	dst := filepath.Join(t.TempDir(), "sub", "usbdisk.img")
	orig := usbDiskImageForTest
	usbDiskImageForTest = dst
	defer func() { usbDiskImageForTest = orig }()

	if !installUsbDisk(bundle) {
		t.Fatal("a missing image was not created")
	}
	if !looksFormatted(dst) {
		t.Error("the created image isn't a filesystem")
	}
}

// An unchanged release must not rewrite the drive: that is an SD-card write on
// every routine update for no reason.
func TestInstallUsbDiskSkipsAnUnchangedBuild(t *testing.T) {
	bundle := t.TempDir()
	gzipFile(t, filepath.Join(bundle, "usbdisk.img.gz"), fatImage())

	dir := t.TempDir()
	dst := filepath.Join(dir, "usbdisk.img")
	origI, origS := usbDiskImageForTest, usbDiskStampForTest
	usbDiskImageForTest, usbDiskStampForTest = dst, filepath.Join(dir, ".usbdisk.stamp")
	defer func() { usbDiskImageForTest, usbDiskStampForTest = origI, origS }()

	if !installUsbDisk(bundle) {
		t.Fatal("first install should write the drive")
	}
	if installUsbDisk(bundle) {
		t.Fatal("the same build was written twice")
	}
}

// The whole reason the stamp exists: a device provisioned once must still pick
// up a fixed launcher, instead of keeping its original drive for good.
func TestInstallUsbDiskRefreshesWhenTheBuildChanges(t *testing.T) {
	bundle := t.TempDir()
	gzipFile(t, filepath.Join(bundle, "usbdisk.img.gz"), fatImage())

	dir := t.TempDir()
	dst := filepath.Join(dir, "usbdisk.img")
	origI, origS := usbDiskImageForTest, usbDiskStampForTest
	usbDiskImageForTest, usbDiskStampForTest = dst, filepath.Join(dir, ".usbdisk.stamp")
	defer func() { usbDiskImageForTest, usbDiskStampForTest = origI, origS }()

	if !installUsbDisk(bundle) {
		t.Fatal("first install should write the drive")
	}
	next := fatImage()
	copy(next[0:9], []byte("BUILD-TWO"))
	gzipFile(t, filepath.Join(bundle, "usbdisk.img.gz"), next)
	if !installUsbDisk(bundle) {
		t.Fatal("a changed build should rewrite the drive")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0:9]) != "BUILD-TWO" {
		t.Fatalf("drive still holds the old build: %q", got[0:9])
	}
}
func TestInstallUsbDiskIgnoresBundleWithout(t *testing.T) {
	if installUsbDisk(t.TempDir()) {
		t.Fatal("claimed to install from a bundle with no image")
	}
}

// Presence is not readiness — the distinction the whole change turns on.
func TestLooksFormatted(t *testing.T) {
	dir := t.TempDir()
	blank := filepath.Join(dir, "blank.img")
	if err := os.WriteFile(blank, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if looksFormatted(blank) {
		t.Error("a zero-filled file read as a filesystem")
	}
	short := filepath.Join(dir, "short.img")
	if err := os.WriteFile(short, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if looksFormatted(short) {
		t.Error("a truncated file read as a filesystem")
	}
	if looksFormatted(filepath.Join(dir, "absent.img")) {
		t.Error("a missing file read as a filesystem")
	}
	good := filepath.Join(dir, "good.img")
	if err := os.WriteFile(good, fatImage(), 0o644); err != nil {
		t.Fatal(err)
	}
	if !looksFormatted(good) {
		t.Error("a FAT image didn't read as a filesystem")
	}
}
