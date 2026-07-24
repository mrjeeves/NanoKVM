package application

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"NanoKVM-Server/proto"
	"NanoKVM-Server/utils"
)

// Our own firmware release channel — never Sipeed's CDN. The update endpoint
// pulls this single bundle (the same asset `just fetch` uses) from our GitHub
// releases and installs our build, so the fleet stays on our firmware. See
// docs/MESH.md.
const (
	releaseBaseURL = "https://github.com/mrjeeves/NanoKVM/releases"
	bundleAsset    = "nanokvm-mesh-riscv64.tar.gz"

	maxTries = 3
)

// tagPattern bounds an operator-supplied version to a release-tag shape
// (`v0.1.0`) so it can only ever name a release under our own repo — never
// smuggle a path segment or a different host into the download URL.
var tagPattern = regexp.MustCompile(`^v[0-9][0-9A-Za-z.\-]*$`)

// updateRequest is the optional POST body: which release to install. Empty or
// "latest" installs the newest published release.
type updateRequest struct {
	Version string `json:"version"`
}

// Update installs our firmware release, pulled from our GitHub release
// channel (never cdn.sipeed.com — a stock update would clobber our mesh
// server build; see docs/MESH.md and the removed stock update service).
//
// It sits behind the normal CheckToken gate, which means the AllMyStuff mesh
// tunnel authorizes it with **no device password** (mesh-roster membership is
// the auth — the whole point of reaching a KVM over the mesh), while a direct
// LAN caller still needs the KVM login. Either way it pulls our release bundle
// for `version` (default: latest), verifies its sha256, installs the server +
// web over /kvmapp (and the pinned myownmesh daemon when the bundle carries a
// changed one), and restarts.
func (s *Service) Update(c *gin.Context) {
	var rsp proto.Response

	var req updateRequest
	_ = c.ShouldBindJSON(&req) // body is optional; empty = latest

	version := strings.TrimSpace(req.Version)
	if version != "" && version != "latest" && !tagPattern.MatchString(version) {
		rsp.ErrRsp(c, -1, "invalid version; expected a release tag like v0.1.0")
		return
	}

	if !acquireUpdateLock() {
		rsp.ErrRsp(c, -1, "update already in progress")
		return
	}
	defer releaseUpdateLock()

	daemonChanged, err := runChannelUpdate(version)
	if err != nil {
		rsp.ErrRsp(c, -1, fmt.Sprintf("update failed: %s", err))
		return
	}

	rsp.OkRsp(c)
	log.Infof("firmware update to %q applied; restarting", versionLabel(version))

	// Answer first, then restart: the OK rides back over the mesh tunnel
	// before the server (and its bridge) bounce. The caller's connection drops
	// and re-establishes on the new build.
	time.Sleep(1 * time.Second)

	// A daemon bump is the one case a routine update restarts the daemon, and it
	// MUST go before the server restart below: `S95nanokvm restart` kills this
	// very process (killall NanoKVM-Server), so anything after it never runs.
	// Restarting the daemon drops the mesh tunnel the update rode in on — fine
	// now that the OK is flushed — and it's the only way a daemon-side fix (e.g.
	// a mesh-connectivity fix) reaches the device, which the server + web swap
	// alone can't deliver. Skipped when the daemon didn't change, so an ordinary
	// update leaves the daemon and its tunnel untouched.
	if daemonChanged {
		log.Infof("bundled myownmesh daemon changed; restarting daemon")
		_ = exec.Command("sh", "-c", "/etc/init.d/S94myownmesh restart").Run()
	}
	_ = exec.Command("sh", "-c", "/etc/init.d/S95nanokvm restart").Run()
}

func versionLabel(version string) string {
	if version == "" || version == "latest" {
		return "latest"
	}
	return version
}

// bundleURL builds the download URL for our release bundle at `version`
// ("" / "latest" → the newest release). GitHub's /latest/download and
// /download/<tag> paths both redirect to the asset; utils.Download follows it.
func bundleURL(version, asset string) string {
	if version == "" || version == "latest" {
		return fmt.Sprintf("%s/latest/download/%s", releaseBaseURL, asset)
	}
	return fmt.Sprintf("%s/download/%s/%s", releaseBaseURL, version, asset)
}

// runChannelUpdate downloads, verifies, and installs the release bundle, and
// reports whether the bundled myownmesh daemon was replaced (so the caller can
// restart it).
func runChannelUpdate(version string) (bool, error) {
	_ = os.RemoveAll(CacheDir)
	if err := os.MkdirAll(CacheDir, 0o755); err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(CacheDir) }()

	// Download the bundle + its published sha256.
	url := bundleURL(version, bundleAsset)
	bundlePath := filepath.Join(CacheDir, bundleAsset)
	if err := downloadWithRetry(url, bundlePath); err != nil {
		return false, fmt.Errorf("download bundle: %w", err)
	}
	shaPath := bundlePath + ".sha256"
	if err := downloadWithRetry(url+".sha256", shaPath); err != nil {
		return false, fmt.Errorf("download checksum: %w", err)
	}

	// A firmware install MUST verify integrity — no soft-fail on a bad or
	// missing checksum.
	if err := verifySha256(bundlePath, shaPath); err != nil {
		return false, fmt.Errorf("checksum: %w", err)
	}

	// Extract and install the server + web over /kvmapp (and the daemon if it
	// changed).
	extractDir := filepath.Join(CacheDir, "bundle")
	if _, err := utils.UnTarGz(bundlePath, extractDir); err != nil {
		return false, fmt.Errorf("extract: %w", err)
	}
	daemonChanged, err := installBundle(extractDir, AppDir)
	if err != nil {
		return false, fmt.Errorf("install: %w", err)
	}
	return daemonChanged, nil
}

// downloadWithRetry fetches url to target, retrying a few times (a release
// asset can 5xx on a cold CDN edge).
func downloadWithRetry(url, target string) error {
	var err error
	for i := 0; i < maxTries; i++ {
		if i > 0 {
			time.Sleep(3 * time.Second)
		}
		var req *http.Request
		req, err = http.NewRequest("GET", url, nil)
		if err != nil {
			continue
		}
		if err = utils.Download(req, target); err == nil {
			return nil
		}
	}
	return err
}

// verifySha256 checks bundlePath against a `sha256sum`-format file (the
// `<hex>  <name>` line our release CI publishes as `<asset>.sha256`).
func verifySha256(bundlePath, shaFilePath string) error {
	want, err := expectedSha256(shaFilePath)
	if err != nil {
		return err
	}
	f, err := os.Open(bundlePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, want)
	}
	return nil
}

// expectedSha256 reads the first hex field of a sha256sum-format file.
func expectedSha256(shaFilePath string) (string, error) {
	raw, err := os.ReadFile(shaFilePath)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", fmt.Errorf("malformed sha256 file")
	}
	return fields[0], nil
}

// filesDiffer reports whether the file at b differs from the file at a by
// sha256 content. A missing b counts as "differs" (a first install must place
// it); any other error opening/reading either file is surfaced.
func filesDiffer(a, b string) (bool, error) {
	sumB, err := sha256File(b)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	sumA, err := sha256File(a)
	if err != nil {
		return false, err
	}
	return !strings.EqualFold(sumA, sumB), nil
}

// sha256File returns the hex-encoded sha256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// installBundle places our build's server binary and web tree from the
// extracted bundle into appDir (/kvmapp on device), atomically and with a
// scoped backup so a mid-swap failure rolls all parts back together.
//
// It also swaps in the pinned myownmesh daemon the bundle carries, but ONLY
// when that binary differs from the one already installed — the common
// server-only update leaves the daemon (and the mesh tunnel it serves)
// completely alone. It returns whether the daemon was replaced so the caller
// can restart it: a daemon-side fix (e.g. a mesh-connectivity fix) rides
// exactly this path, since the server + web swap alone can't deliver one. A
// bundle with no daemon (older layout) simply skips it.
func installBundle(bundleDir, appDir string) (bool, error) {
	serverSrc := filepath.Join(bundleDir, "NanoKVM-Server")
	webSrc := filepath.Join(bundleDir, "web")
	if !isFile(serverSrc) {
		return false, fmt.Errorf("bundle is missing NanoKVM-Server")
	}
	if !isDir(webSrc) {
		return false, fmt.Errorf("bundle is missing web/")
	}

	serverDir := filepath.Join(appDir, "server")
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		return false, err
	}
	serverDst := filepath.Join(serverDir, "NanoKVM-Server")
	webDst := filepath.Join(serverDir, "web")
	serverBackup := serverDst + ".old"
	webBackup := webDst + ".old"

	// Decide the daemon up front (a read-only sha256 compare, no mutation): swap
	// the bundled myownmesh in only when it differs from the installed binary,
	// so an unchanged daemon is never rewritten and the caller never restarts
	// it. A bundle without the daemon just skips it rather than failing.
	daemonDst := filepath.Join(appDir, "system", "bin", "myownmesh")
	daemonSrc := filepath.Join(bundleDir, "myownmesh")
	daemonBackup := daemonDst + ".old"
	swapDaemon := false
	if isFile(daemonSrc) {
		differs, err := filesDiffer(daemonSrc, daemonDst)
		if err != nil {
			return false, fmt.Errorf("compare daemon: %w", err)
		}
		swapDaemon = differs
	}

	// --- daemon binary (only when changed): stage, back up, swap in ---
	// Done first so a failure here leaves the server + web untouched. Renaming
	// over the live daemon is safe: on the Pro it keeps the old inode until
	// systemd restarts it, and on the NanoKVM the running daemon is a tmpfs copy
	// re-staged from this path on restart — so neither hits ETXTBSY the way an
	// in-place copy of a running binary would.
	if swapDaemon {
		if err := os.MkdirAll(filepath.Dir(daemonDst), 0o755); err != nil {
			return false, err
		}
		daemonStage := daemonDst + ".new"
		if err := copyFileMode(daemonSrc, daemonStage, 0o755); err != nil {
			return false, err
		}
		_ = os.Remove(daemonBackup)
		if isFile(daemonDst) {
			if err := os.Rename(daemonDst, daemonBackup); err != nil {
				_ = os.Remove(daemonStage)
				return false, err
			}
		}
		if err := os.Rename(daemonStage, daemonDst); err != nil {
			_ = os.Remove(daemonStage)
			restore(daemonBackup, daemonDst)
			return false, err
		}
	}

	// --- server binary: stage beside, back up the current, swap in ---
	// Rename replaces a running executable fine (the live process keeps the old
	// inode until it exits), so this is safe even where the server runs in
	// place.
	serverStage := serverDst + ".new"
	if err := copyFileMode(serverSrc, serverStage, 0o755); err != nil {
		if swapDaemon {
			restore(daemonBackup, daemonDst)
		}
		return false, err
	}
	_ = os.Remove(serverBackup)
	if isFile(serverDst) {
		if err := os.Rename(serverDst, serverBackup); err != nil {
			_ = os.Remove(serverStage)
			if swapDaemon {
				restore(daemonBackup, daemonDst)
			}
			return false, err
		}
	}
	if err := os.Rename(serverStage, serverDst); err != nil {
		restore(serverBackup, serverDst)
		if swapDaemon {
			restore(daemonBackup, daemonDst)
		}
		return false, err
	}

	// --- web tree: stage web.new, swap over web, keep web.old until done ---
	// The stage lands on the same filesystem as web (/kvmapp/server), so the
	// swap-in rename is atomic; getting the bundle's web there is a per-file
	// cross-fs move from the cache.
	webStage := webDst + ".new"
	_ = os.RemoveAll(webStage)
	_ = os.RemoveAll(webBackup)
	if err := utils.MoveFilesRecursively(webSrc, webStage); err != nil {
		_ = os.RemoveAll(webStage)
		// All parts move together: undo the server (and daemon) swap so we never
		// leave a new server with an old web.
		restore(serverBackup, serverDst)
		if swapDaemon {
			restore(daemonBackup, daemonDst)
		}
		return false, err
	}
	if isDir(webDst) {
		if err := os.Rename(webDst, webBackup); err != nil {
			_ = os.RemoveAll(webStage)
			restore(serverBackup, serverDst)
			if swapDaemon {
				restore(daemonBackup, daemonDst)
			}
			return false, err
		}
	}
	if err := os.Rename(webStage, webDst); err != nil {
		if isDir(webBackup) {
			_ = os.Rename(webBackup, webDst)
		}
		_ = os.RemoveAll(webStage)
		restore(serverBackup, serverDst)
		if swapDaemon {
			restore(daemonBackup, daemonDst)
		}
		return false, err
	}

	// Success — drop the backups and normalize modes.
	_ = os.Remove(serverBackup)
	_ = os.RemoveAll(webBackup)
	if swapDaemon {
		_ = os.Remove(daemonBackup)
	}
	if err := chmodTree(webDst, 0o755); err != nil {
		return false, err
	}
	return swapDaemon, nil
}

// chmodTree sets mode on root and everything under it — the served web tree,
// so a bundle with odd perms can't leave an asset unreadable.
func chmodTree(root string, mode os.FileMode) error {
	return filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chmod(path, mode)
	})
}

// restore moves a `.old` backup back over its destination after a failed swap.
func restore(backup, dst string) {
	if isFile(backup) || isDir(backup) {
		_ = os.Rename(backup, dst)
	}
}

func copyFileMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
