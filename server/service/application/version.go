package application

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"NanoKVM-Server/buildinfo"
	"NanoKVM-Server/proto"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// githubLatestAPI is our release channel's "latest release" endpoint. We read
// the tag name to tell the Update tab whether a newer build is out — the same
// channel the Update handler installs from, never cdn.sipeed.com.
const githubLatestAPI = "https://api.github.com/repos/mrjeeves/NanoKVM/releases/latest"

// GetVersion reports the running firmware version and, best-effort, the latest
// version on our release channel (so the Update tab can show "up to date" vs
// "update available"). The current version is OUR fork's version (buildinfo) —
// NOT the Sipeed base image's /kvmapp/version, which is an unrelated upstream
// 2.x and would make every comparison read "up to date". A failed
// latest-lookup (no internet, rate limit) just omits `latest`, so the tab
// treats the device as current rather than nagging.
func (s *Service) GetVersion(c *gin.Context) {
	var rsp proto.Response

	currentVersion := buildinfo.Version
	log.Debugf("current version: %s", currentVersion)

	// Keep the REASON a lookup failed, don't just blank the version. An empty
	// `latest` alone is indistinguishable from "you're current", and that is
	// exactly how it was read: a device that couldn't reach the channel
	// reported "You already have the latest version", so a shipped release
	// looked like one that never shipped and the search started in the wrong
	// place. The likeliest cause is worth naming too — this call is
	// unauthenticated, and GitHub allows 60 of those per hour per IP, so a
	// device (or a house with two of them) that checks often enough gets a 403.
	// `?refresh=1` is the explicit "Check for Updates" press; everything else
	// takes the cached answer.
	latestVersion, latestError := cachedLatest(c.Query("refresh") == "1")

	rsp.OkRspWithData(c, &proto.GetVersionRsp{
		Current:     currentVersion,
		Latest:      latestVersion,
		LatestError: latestError,
	})
}

// The release-channel answer is cached. It used to be fetched synchronously on
// every single call — a 10 s-timeout network round-trip in a request path, run
// on every page load of the update tab and every Update press in the app. That
// is slow where it is least wanted, and it spends a budget that is smaller than
// it looks: this call is unauthenticated, and GitHub allows 60 of those per
// hour per IP, shared by every device behind it. A house with two KVMs and an
// app that checks can burn through that and start getting 403s, at which point
// the device stops learning about releases for reasons that have nothing to do
// with the release.
//
// A TTL, not a refresh loop: nothing here needs to know about a release before
// someone asks. An explicit "Check for Updates" bypasses it (see GetVersion),
// so the button still means what it says — the cache only serves the incidental
// callers.
//
// The lookup happens under the lock on purpose. Concurrent callers coalesce
// onto one request instead of racing to spend the same budget, which is exactly
// the behaviour that keeps a burst from costing more than a single check.
const latestCacheTTL = 10 * time.Minute

var latestCache struct {
	mu      sync.Mutex
	version string
	err     string
	at      time.Time
}

// cachedLatest returns the channel's newest tag and, if the lookup failed, why.
// `force` skips the cache for an explicitly requested check.
func cachedLatest(force bool) (string, string) {
	latestCache.mu.Lock()
	defer latestCache.mu.Unlock()

	if !force && !latestCache.at.IsZero() && time.Since(latestCache.at) < latestCacheTTL {
		return latestCache.version, latestCache.err
	}

	version, errText := "", ""
	if latest, err := latestChannelVersion(); err == nil && latest != "" {
		version = latest
	} else if err != nil {
		errText = err.Error()
		log.Infof("update check: couldn't reach the release channel: %s", err)
	}
	latestCache.version, latestCache.err, latestCache.at = version, errText, time.Now()
	return version, errText
}

// latestChannelVersion asks GitHub for our newest release's tag and returns it
// without the leading "v", so it compares cleanly against the current version
// read from /kvmapp/version.
func latestChannelVersion() (string, error) {
	req, err := http.NewRequest("GET", githubLatestAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status code %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(release.TagName), "v"), nil
}
