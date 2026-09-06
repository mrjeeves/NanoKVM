package mesh

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"NanoKVM-Server/config"
	"NanoKVM-Server/middleware"
	"github.com/gin-gonic/gin"
)

func TestSupportHTTPConsent(t *testing.T) {
	conf := config.GetInstance()
	previous := conf.Authentication
	conf.Authentication = "enable"
	t.Cleanup(func() { conf.Authentication = previous })
	b := &Bridge{state: LoadState("")}
	b.state.TryClaim("owner", "Owner")
	router := gin.New()
	RegisterRoutes(router, b)
	call := func(path, body string, authed bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		if authed {
			meshAuthHandler{engine: router, peer: "owner"}.ServeHTTP(rec, req)
		} else {
			router.ServeHTTP(rec, req)
		}
		return rec
	}
	code := func(rec *httptest.ResponseRecorder) int {
		t.Helper()
		var response struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.Code
	}
	for _, path := range []string{"arm", "approve", "deny"} {
		if got := call("/api/mesh/help/"+path, `{}`, false).Code; got != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated: %d", path, got)
		}
	}
	if _, left := b.cecSupportSnapshot(time.Now()); left != 0 {
		t.Fatal("unauthenticated caller opened window")
	}
	for _, path := range []string{"raise", "lower", "toggle"} {
		if got := call("/api/mesh/help/"+path, `{}`, true).Code; got != http.StatusNotFound {
			t.Fatalf("retired endpoint %s still exists: %d", path, got)
		}
	}
	if code(call("/api/mesh/help/arm", `{}`, true)) != 0 {
		t.Fatal("authenticated arm failed")
	}
	if _, left := b.cecSupportSnapshot(time.Now()); left < 299 {
		t.Fatal("arm did not start five-minute window")
	}
	requestSupport(b, "first", "s1")
	if !b.cecApprovedTech("first") {
		t.Fatal("armed request did not gain access")
	}
	requestSupport(b, "second", "s2")
	for _, body := range []string{`{`, `{}`, `{"technician":"second","sessionId":"stale"}`} {
		if code(call("/api/mesh/help/approve", body, true)) == 0 {
			t.Fatalf("invalid decision accepted: %s", body)
		}
	}
	if b.cecApprovedTech("second") {
		t.Fatal("stale decision granted access")
	}
	if code(call("/api/mesh/help/approve", `{"technician":"second","sessionId":"s2"}`, true)) != 0 {
		t.Fatal("matching decision failed")
	}
	if !b.cecApprovedTech("second") {
		t.Fatal("matching decision did not grant access")
	}
	requestSupport(b, "third", "s3")
	if code(call("/api/mesh/help/deny", `{"technician":"third","sessionId":"s3"}`, true)) != 0 {
		t.Fatal("decline failed")
	}
	if pending, _ := b.cecSupportSnapshot(time.Now()); len(pending) != 0 {
		t.Fatal("decided request still pending")
	}
}

func TestEmptyNetworkCannotReceiveSupportRequest(t *testing.T) {
	b := &Bridge{state: LoadState("")}
	b.handleCecControl("", "tech", []byte(`{"t":"connect","kind":"request","session_id":"s1"}`))
	if pending, _ := b.cecSupportSnapshot(time.Now()); len(pending) != 0 {
		t.Fatal("empty session room accepted request")
	}
}

func TestTemporarySupportCannotApproveMoreAccess(t *testing.T) {
	conf := config.GetInstance()
	previous := conf.Authentication
	conf.Authentication = "enable"
	t.Cleanup(func() { conf.Authentication = previous })
	b := &Bridge{state: LoadState("")}
	b.state.TryClaim("owner", "Owner")
	b.fleetRoster = map[string]struct{}{"fleet-member": {}}
	b.state.GrantCecTech("technician", cecGrantWindow)
	router := gin.New()
	RegisterRoutes(router, b)
	for _, peer := range []string{"technician", "stranger", ""} {
		for _, action := range []string{"arm", "approve", "deny"} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/mesh/help/"+action, strings.NewReader(`{"technician":"technician","sessionId":"s1"}`))
			meshAuthHandler{engine: router, peer: peer}.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%q could %s: %d", peer, action, rec.Code)
			}
		}
		rec := httptest.NewRecorder()
		meshAuthHandler{engine: router, peer: peer}.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mesh/help", nil))
		if !strings.Contains(rec.Body.String(), `"canApprove":false`) {
			t.Fatal("technician was shown approval controls")
		}
	}
	if _, left := b.cecSupportSnapshot(time.Now()); left != 0 {
		t.Fatal("technician opened a new window")
	}
	for _, peer := range []string{"owner-AB12C", "fleet-member"} {
		rec := httptest.NewRecorder()
		meshAuthHandler{engine: router, peer: peer}.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/mesh/help/arm", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("owner/fleet denied: %s", peer)
		}
	}
	// A local authenticated web login can also approve without mesh ownership.
	token, err := middleware.GenerateJWT("local-owner")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/mesh/help/arm", nil)
	request.AddCookie(&http.Cookie{Name: "nano-kvm-token", Value: token})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, request)
	if rec.Code != http.StatusOK {
		t.Fatalf("local login denied: %d", rec.Code)
	}
}
