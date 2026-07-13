package main

// The -web listener's flag/config validation plus the full browser-security
// bypass matrix (P4-design.md WP2), exercised against the REAL, unmodified
// api mux this package builds (server.routes()) — internal/web's own tests
// cover the middleware mechanics in isolation with a stand-in API handler
// (it can't import this "main" package to get the real one); these tests are
// the "via httptest against the real middleware-wrapped mux" pass.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/web"
)

// ---- validateWebAddr / isLoopbackHost ----

func TestValidateWebAddrAcceptsLoopbackForms(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7788", "127.0.0.1:0", "localhost:7788", "[::1]:7788", "127.9.9.9:1"} {
		if err := validateWebAddr(addr); err != nil {
			t.Errorf("validateWebAddr(%q) = %v, want nil (loopback)", addr, err)
		}
	}
}

// TestValidateWebAddrRefusesNonLoopback covers the bypass matrix's exact
// refusal cases: 0.0.0.0, a public/LAN IP, and an arbitrary hostname. Every
// message must name "loopback", "v0.7", and the offending value verbatim.
func TestValidateWebAddrRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:7788", "192.168.1.5:7788", "example.com:7788", "10.0.0.1:1", "8.8.8.8:53"} {
		err := validateWebAddr(addr)
		if err == nil {
			t.Fatalf("validateWebAddr(%q) = nil, want a refusal", addr)
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("validateWebAddr(%q) error = %q, want it to mention loopback", addr, err)
		}
		if !strings.Contains(err.Error(), "v0.7") {
			t.Errorf("validateWebAddr(%q) error = %q, want it to name v0.7", addr, err)
		}
		if !strings.Contains(err.Error(), addr) {
			t.Errorf("validateWebAddr(%q) error = %q, want it to name the offending value", addr, err)
		}
	}
}

// TestValidateWebAddrRefusesMalformedAddress: a bare port with no host at
// all (missing the ":" split net.SplitHostPort needs) must refuse too, not
// panic or silently pass.
func TestValidateWebAddrRefusesMalformedAddress(t *testing.T) {
	if err := validateWebAddr("7788"); err == nil {
		t.Error(`validateWebAddr("7788") = nil, want a refusal (no host:port split)`)
	}
}

func TestIsLoopbackHostAcceptsKnownLoopbackForms(t *testing.T) {
	for _, h := range []string{"127.0.0.1", "127.0.0.2", "localhost", "::1"} {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false, want true", h)
		}
	}
}

func TestIsLoopbackHostRefusesEmptyAndNonLoopback(t *testing.T) {
	for _, h := range []string{"", "0.0.0.0", "192.168.1.5", "example.com", "::"} {
		if isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = true, want false", h)
		}
	}
}

// ---- /api/status webAddr ----

func TestHandleStatusReportsWebAddrWhenSet(t *testing.T) {
	srv, _ := buildTestServer(t)
	srv.webAddr = "127.0.0.1:54321"
	handler := srv.routes()

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got statusPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.WebAddr != "127.0.0.1:54321" {
		t.Errorf("WebAddr = %q, want 127.0.0.1:54321", got.WebAddr)
	}
}

func TestHandleStatusReportsEmptyWebAddrWhenWebIsOff(t *testing.T) {
	srv, _ := buildTestServer(t) // webAddr left at its zero value: -web off
	handler := srv.routes()

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var got statusPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.WebAddr != "" {
		t.Errorf("WebAddr = %q, want empty when -web is off", got.WebAddr)
	}
}

// ---- socket path: stays tokenless ----

// TestSocketMuxAcceptsMutationWithoutAnyCSRFHeader pins the objective's most
// load-bearing regression check: server.routes() — exactly what the unix
// socket serves — must accept a state-changing POST with zero security
// headers at all, unchanged by any of this phase's middleware. wt/agents
// keep working exactly as before; the socket's trust boundary stays
// filesystem permissions.
func TestSocketMuxAcceptsMutationWithoutAnyCSRFHeader(t *testing.T) {
	srv, _ := buildTestServer(t)
	handler := srv.routes()

	req := httptest.NewRequest(http.MethodPost, "/api/refresh", strings.NewReader(""))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (the socket mux must stay tokenless); body=%s", rec.Code, rec.Body.String())
	}
}

// ---- -tcp wrap: web.HostOriginOnly ----

const testTCPAddr = "127.0.0.1:7799"

func TestTCPWrapAllowsHeaderlessCLIShapedPost(t *testing.T) {
	srv, _ := buildTestServer(t)
	handler := web.HostOriginOnly(srv.routes(), testTCPAddr)

	req := httptest.NewRequest(http.MethodPost, "/api/refresh", strings.NewReader(""))
	req.Host = testTCPAddr
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a headerless CLI-shaped POST over -tcp; body=%s", rec.Code, rec.Body.String())
	}
}

func TestTCPWrapRefusesBrowserShapedForeignOriginPost(t *testing.T) {
	srv, _ := buildTestServer(t)
	handler := web.HostOriginOnly(srv.routes(), testTCPAddr)

	req := httptest.NewRequest(http.MethodPost, "/api/refresh", strings.NewReader(""))
	req.Host = testTCPAddr
	req.Header.Set("Origin", "http://evil.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a foreign-Origin POST over -tcp; body=%s", rec.Code, rec.Body.String())
	}
}

func TestTCPWrapRefusesWrongHost(t *testing.T) {
	srv, _ := buildTestServer(t)
	handler := web.HostOriginOnly(srv.routes(), testTCPAddr)

	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	req.Host = "evil.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for the wrong Host over -tcp; body=%s", rec.Code, rec.Body.String())
	}
}

// ---- -web listener: the full bypass matrix against the real mux ----

const testWebAddr = "127.0.0.1:7788"

// buildTestWebApp composes the -web handler exactly the way main() does:
// server.routes() (untouched) wrapped by web.New's full browser-security
// stack. No real net.Listen happens; httptest drives the handler directly.
func buildTestWebApp(t *testing.T) (http.Handler, string) {
	t.Helper()
	srv, _ := buildTestServer(t)
	token := web.NewCSRFToken()
	h := web.New(srv.eng, srv.routes(), web.Config{BoundAddr: testWebAddr, CSRFToken: token})
	return h, token
}

func webReq(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Host = testWebAddr
	return req
}

// TestWebListenerRefusesWrongHostOnEveryRouteKind: pages, the fragment, the
// API, and SSE all sit behind the same Host allowlist — none of them may be
// reachable with a foreign Host.
func TestWebListenerRefusesWrongHostOnEveryRouteKind(t *testing.T) {
	h, _ := buildTestWebApp(t)
	for _, path := range []string{"/", "/wt/some-id", "/fragment/worktrees", "/api/version", "/api/events"} {
		req := webReq(http.MethodGet, path)
		req.Host = "evil.com"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with Host: evil.com: status = %d, want 403; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestWebListenerRefusesMissingHost(t *testing.T) {
	h, _ := buildTestWebApp(t)
	req := webReq(http.MethodGet, "/")
	req.Host = ""
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a missing Host header; body=%s", rec.Code, rec.Body.String())
	}
}

// TestWebListenerRefusesDNSRebindingShapedHost: Host: attacker.com with an
// otherwise perfectly legitimate (loopback-bound) request — the exact shape
// of a successful DNS-rebinding attack on the wire.
func TestWebListenerRefusesDNSRebindingShapedHost(t *testing.T) {
	h, _ := buildTestWebApp(t)
	req := webReq(http.MethodGet, "/")
	req.Host = "attacker.com:7788"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWebListenerRefusesForeignOriginOnGET(t *testing.T) {
	h, _ := buildTestWebApp(t)
	req := webReq(http.MethodGet, "/")
	req.Header.Set("Origin", "http://evil.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWebListenerRefusesForeignOriginOnPOST(t *testing.T) {
	h, _ := buildTestWebApp(t)
	req := webReq(http.MethodPost, "/api/refresh")
	req.Header.Set("Origin", "http://evil.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestWebListenerRefusesSSEFromEvilOrigin is the cross-site-SSE-read bypass
// case: /api/events is a GET, but a foreign Origin must still be refused.
func TestWebListenerRefusesSSEFromEvilOrigin(t *testing.T) {
	h, _ := buildTestWebApp(t)
	req := webReq(http.MethodGet, "/api/events")
	req.Header.Set("Origin", "http://evil.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestWebListenerSSEWithGoodHostDeliversHelloFrame is the control case: a
// good Host (no Origin, as a real EventSource same-origin connection sends)
// reaches the real handleEvents and gets the hello preamble first, exactly
// as it does over the socket (see main_test.go's
// TestHandleEventsEmitsHelloEventFirst) — proving the web wrap doesn't just
// refuse everything. Same already-cancelled-context trick as that test:
// handleEvents writes hello+snapshot unconditionally, then its for-select's
// ctx.Done() case fires immediately — otherwise ServeHTTP never returns
// (handleEvents only exits when the request context ends).
func TestWebListenerSSEWithGoodHostDeliversHelloFrame(t *testing.T) {
	h, _ := buildTestWebApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := webReq(http.MethodGet, "/api/events").WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Body.String(), "event: hello") {
		t.Errorf("body does not start with the hello frame:\n%s", rec.Body.String())
	}
}

func TestWebListenerRefusesPostWithoutCSRFToken(t *testing.T) {
	h, _ := buildTestWebApp(t)
	req := webReq(http.MethodPost, "/api/refresh")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "csrf token mismatch — reload the page" {
		t.Errorf("body = %q, want the exact reload message", got)
	}
}

// TestWebListenerAcceptsPostWithCSRFTokenScrapedFromPageMeta is the
// end-to-end token lifecycle: GET the index, scrape <meta name="csrf-token">,
// then use exactly that value as X-Csrf-Token on a mutating POST.
func TestWebListenerAcceptsPostWithCSRFTokenScrapedFromPageMeta(t *testing.T) {
	h, token := buildTestWebApp(t)

	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, webReq(http.MethodGet, "/"))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200; body=%s", getRec.Code, getRec.Body.String())
	}
	scraped := scrapeMetaCSRFToken(getRec.Body.String())
	if scraped == "" {
		t.Fatalf("no csrf-token meta tag found in:\n%s", getRec.Body.String())
	}
	if scraped != token {
		t.Fatalf("scraped token = %q, want %q", scraped, token)
	}

	postReq := webReq(http.MethodPost, "/api/refresh")
	postReq.Header.Set("X-Csrf-Token", scraped)
	postRec := httptest.NewRecorder()
	h.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusOK {
		t.Errorf("POST with the scraped token: status = %d, want 200; body=%s", postRec.Code, postRec.Body.String())
	}
}

func TestWebListenerCSPNosniffFrameHeadersExactMatch(t *testing.T) {
	h, _ := buildTestWebApp(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, webReq(http.MethodGet, "/"))

	want := map[string]string{
		"Content-Security-Policy": "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

var metaCSRFRE = regexp.MustCompile(`<meta name="csrf-token" content="([^"]*)">`)

func scrapeMetaCSRFToken(html string) string {
	m := metaCSRFRE.FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	return m[1]
}
