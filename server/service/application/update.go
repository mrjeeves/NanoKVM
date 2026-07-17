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
// web over /kvmapp, and restarts.
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

	if err := runChannelUpdate(version); err != nil {
		rsp.ErrRsp(c, -1, fmt.Sprintf("update failed: %s", err))
		return
	}

	rsp.OkRsp(c)
	log.Infof("firmware update to %q applied; restarting", versionLabel(version))

	// Answer first, then restart: the OK rides back over the mesh tunnel
	// before the server (and its bridge) bounce. The caller's connection drops
	// and re-establishes on the new build.
	time.Sleep(1 * time.Second)
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

func runChannelUpdate(version string) error {
	_ = os.RemoveAll(CacheDir)
	if err := os.MkdirAll(CacheDir, 0o755); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(CacheDir) }()

	// Download the bundle + its published sha256.
	url := bundleURL(version, bundleAsset)
	bundlePath := filepath.Join(CacheDir, bundleAsset)
	if err := downloadWithRetry(url, bundlePath); err != nil {
		return fmt.Errorf("download bundle: %w", err)
	}
	shaPath := bundlePath + ".sha256"
	if err := downloadWithRetry(url+".sha256", shaPath); err != nil {
		return fmt.Errorf("download checksum: %w", err)
	}

	// A firmware install MUST verify integrity — no soft-fail on a bad or
	// missing checksum.
	if err := verifySha256(bundlePath, shaPath); err != nil {
		return fmt.Errorf("checksum: %w", err)
	}

	// Extract and install the server + web over /kvmapp.
	extractDir := filepath.Join(CacheDir, "bundle")
	if _, err := utils.UnTarGz(bundlePath, extractDir); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	if err := installBundle(extractDir, AppDir); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	return nil
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

// installBundle places our build's server binary and web tree from the
// extracted bundle into appDir (/kvmapp on device), atomically and with a
// scoped backup so a mid-swap failure rolls both halves back together.
//
// It deliberately does NOT touch the myownmesh daemon binary the bundle also
// carries: the daemon is pinned per release and a new server runs against the
// existing daemon's stable control socket, so a routine update ships only the
// server + web (what a stock OTA clobbers, and what carries our features). A
// daemon bump rides a full on-site `just deploy`, never this endpoint —
// restarting the daemon here would drop the mesh tunnel mid-update.
func installBundle(bundleDir, appDir string) error {
	serverSrc := filepath.Join(bundleDir, "NanoKVM-Server")
	webSrc := filepath.Join(bundleDir, "web")
	if !isFile(serverSrc) {
		return fmt.Errorf("bundle is missing NanoKVM-Server")
	}
	if !isDir(webSrc) {
		return fmt.Errorf("bundle is missing web/")
	}

	serverDir := filepath.Join(appDir, "server")
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		return err
	}
	serverDst := filepath.Join(serverDir, "NanoKVM-Server")
	webDst := filepath.Join(serverDir, "web")
	serverBackup := serverDst + ".old"
	webBackup := webDst + ".old"

	// --- server binary: stage beside, back up the current, swap in ---
	// Rename replaces a running executable fine (the live process keeps the old
	// inode until it exits), so this is safe even where the server runs in
	// place.
	serverStage := serverDst + ".new"
	if err := copyFileMode(serverSrc, serverStage, 0o755); err != nil {
		return err
	}
	_ = os.Remove(serverBackup)
	if isFile(serverDst) {
		if err := os.Rename(serverDst, serverBackup); err != nil {
			_ = os.Remove(serverStage)
			return err
		}
	}
	if err := os.Rename(serverStage, serverDst); err != nil {
		restore(serverBackup, serverDst)
		return err
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
		// Both halves move together: undo the server swap so we never leave a
		// new server with an old web.
		restore(serverBackup, serverDst)
		return err
	}
	if isDir(webDst) {
		if err := os.Rename(webDst, webBackup); err != nil {
			_ = os.RemoveAll(webStage)
			restore(serverBackup, serverDst)
			return err
		}
	}
	if err := os.Rename(webStage, webDst); err != nil {
		if isDir(webBackup) {
			_ = os.Rename(webBackup, webDst)
		}
		_ = os.RemoveAll(webStage)
		restore(serverBackup, serverDst)
		return err
	}

	// Success — drop the backups and normalize modes.
	_ = os.Remove(serverBackup)
	_ = os.RemoveAll(webBackup)
	if err := chmodTree(webDst, 0o755); err != nil {
		return err
	}
	return nil
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
