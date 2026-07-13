package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// okHandler is the trivial "next" every middleware test wraps: it proves a
// request actually reached the end of the chain (as opposed to a middleware
// having swallowed it silently) by writing a recognisable 200 body.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
})

const testBoundAddr = "127.0.0.1:7788"

func newReq(t *testing.T, method, path string) *http.Request {
	t.Helper()
	return httptest.NewRequest(method, path, nil)
}

// ---- hostAllowed ----

func TestHostAllowedAcceptsBoundAddressLocalhostAndIPv6Loopback(t *testing.T) {
	h := hostAllowed(testBoundAddr, okHandler)
	for _, host := range []string{"127.0.0.1:7788", "localhost:7788", "[::1]:7788"} {
		req := newReq(t, http.MethodGet, "/")
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q: status = %d, want 200 (body=%s)", host, rec.Code, rec.Body.String())
		}
	}
}

// TestHostAllowedRefusesEvilHost is the plain bypass-matrix case: a Host
// header naming an attacker-controlled domain must never reach the handler.
func TestHostAllowedRefusesEvilHost(t *testing.T) {
	h := hostAllowed(testBoundAddr, okHandler)
	req := newReq(t, http.MethodGet, "/")
	req.Host = "evil.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHostAllowedRefusesMissingHost covers the other half of the bypass
// matrix's "wrong/missing Host header": an empty Host must be refused, not
// treated as some permissive default.
func TestHostAllowedRefusesMissingHost(t *testing.T) {
	h := hostAllowed(testBoundAddr, okHandler)
	req := newReq(t, http.MethodGet, "/")
	req.Host = ""
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a missing Host; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHostAllowedRefusesDNSRebindingShapedHost pins the actual threat
// hostAllowed exists for (§1.3): even though this request is (by
// construction, over httptest) indistinguishable at the transport level from
// one arriving over a genuinely loopback-bound socket — exactly what a
// successful DNS-rebinding attack looks like on the wire — a Host header
// naming the attacker's domain is refused purely because hostAllowed only
// ever trusts the header string, never any notion of "where this connection
// really came from".
func TestHostAllowedRefusesDNSRebindingShapedHost(t *testing.T) {
	h := hostAllowed(testBoundAddr, okHandler)
	req := newReq(t, http.MethodGet, "/")
	req.Host = "attacker.com:7788"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHostAllowedRefusesWrongPortOnOtherwiseGoodHost(t *testing.T) {
	h := hostAllowed(testBoundAddr, okHandler)
	req := newReq(t, http.MethodGet, "/")
	req.Host = "127.0.0.1:9999"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for the wrong port; body=%s", rec.Code, rec.Body.String())
	}
}

// ---- originAllowed ----

func TestOriginAllowedPassesRequestsWithNoOriginHeader(t *testing.T) {
	h := originAllowed(testBoundAddr, okHandler)
	req := newReq(t, http.MethodPost, "/api/refresh") // no Origin set: a CLI/agent-shaped request
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a request with no Origin header; body=%s", rec.Code, rec.Body.String())
	}
}

func TestOriginAllowedAcceptsMatchingOrigin(t *testing.T) {
	h := originAllowed(testBoundAddr, okHandler)
	req := newReq(t, http.MethodPost, "/api/refresh")
	req.Header.Set("Origin", "http://127.0.0.1:7788")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for the listener's own origin; body=%s", rec.Code, rec.Body.String())
	}
}

// TestOriginAllowedRefusesForeignOriginOnGET covers the cross-site-SSE-read
// shape of the bypass matrix: a GET carrying a foreign Origin (what a
// same-origin-policy-respecting browser sends for a cross-origin
// fetch/EventSource) must be refused even though the method itself is
// read-only.
func TestOriginAllowedRefusesForeignOriginOnGET(t *testing.T) {
	h := originAllowed(testBoundAddr, okHandler)
	req := newReq(t, http.MethodGet, "/api/events")
	req.Header.Set("Origin", "http://evil.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a foreign Origin on GET /api/events (the SSE-read bypass case); body=%s", rec.Code, rec.Body.String())
	}
}

func TestOriginAllowedRefusesForeignOriginOnPOST(t *testing.T) {
	h := originAllowed(testBoundAddr, okHandler)
	req := newReq(t, http.MethodPost, "/api/refresh")
	req.Header.Set("Origin", "http://evil.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestOriginAllowedRefusesCrossSiteSecFetchSite(t *testing.T) {
	h := originAllowed(testBoundAddr, okHandler)
	req := newReq(t, http.MethodGet, "/")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for Sec-Fetch-Site: cross-site; body=%s", rec.Code, rec.Body.String())
	}
}

func TestOriginAllowedAcceptsSameOriginOrNoneSecFetchSite(t *testing.T) {
	h := originAllowed(testBoundAddr, okHandler)
	for _, v := range []string{"same-origin", "none"} {
		req := newReq(t, http.MethodGet, "/")
		req.Header.Set("Sec-Fetch-Site", v)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Sec-Fetch-Site %q: status = %d, want 200; body=%s", v, rec.Code, rec.Body.String())
		}
	}
}

// ---- csrfRequired ----

func TestCSRFRequiredRefusesPostWithoutToken(t *testing.T) {
	h := csrfRequired("secret-token", okHandler)
	req := newReq(t, http.MethodPost, "/api/refresh")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != csrfMismatchMsg {
		t.Errorf("body = %q, want the exact reload message %q", got, csrfMismatchMsg)
	}
}

func TestCSRFRequiredRefusesPostWithWrongToken(t *testing.T) {
	h := csrfRequired("secret-token", okHandler)
	req := newReq(t, http.MethodPost, "/api/refresh")
	req.Header.Set("X-Csrf-Token", "not-the-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCSRFRequiredAcceptsPostWithCorrectToken(t *testing.T) {
	h := csrfRequired("secret-token", okHandler)
	req := newReq(t, http.MethodPost, "/api/refresh")
	req.Header.Set("X-Csrf-Token", "secret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCSRFRequiredNeverRequiresTokenOnGetOrHead pins that the SSE endpoint
// (a GET) and any other read stay reachable without the header at all —
// EventSource cannot set custom headers.
func TestCSRFRequiredNeverRequiresTokenOnGetOrHead(t *testing.T) {
	h := csrfRequired("secret-token", okHandler)
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		req := newReq(t, m, "/api/events")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("method %s: status = %d, want 200 (no token required); body=%s", m, rec.Code, rec.Body.String())
		}
	}
}

// TestCSRFRequiredWithEmptyConfiguredTokenRefusesEverything guards against
// an empty cfg.CSRFToken (should never happen — New's caller always supplies
// one) silently degrading into "no token required" via an empty == empty
// compare.
func TestCSRFRequiredWithEmptyConfiguredTokenRefusesEverything(t *testing.T) {
	h := csrfRequired("", okHandler)
	req := newReq(t, http.MethodPost, "/api/refresh")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when no token is configured at all; body=%s", rec.Code, rec.Body.String())
	}
}

// ---- secHeaders ----

func TestSecHeadersExactMatch(t *testing.T) {
	h := secHeaders(okHandler)
	req := newReq(t, http.MethodGet, "/")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

// TestSecHeadersPresentOnARefusedDownstreamResponse pins that the headers
// survive even when a later middleware in the chain (secure() puts
// secHeaders outermost) refuses the request.
func TestSecHeadersPresentOnARefusedDownstreamResponse(t *testing.T) {
	refuse := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	h := secHeaders(refuse)
	req := newReq(t, http.MethodGet, "/")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("precondition: status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff header missing on a refused response: %q", got)
	}
}

// ---- secure(): ordering ----

// TestSecureChecksHostBeforeOrigin pins the documented ordering: a request
// with BOTH a wrong Host and a wrong Origin must be refused for the Host
// reason (hostAllowed runs first and short-circuits), not silently pass
// through to the Origin check.
func TestSecureChecksHostBeforeOrigin(t *testing.T) {
	h := secure(okHandler, Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})
	req := newReq(t, http.MethodGet, "/")
	req.Host = "evil.com"
	req.Header.Set("Origin", "http://also-evil.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "host not allowed" {
		t.Errorf("body = %q, want the host-rejection message (proving Host is checked before Origin)", got)
	}
}

// TestSecureChecksOriginAndHostBeforeCSRF: a same-origin-shaped POST that
// still lacks a CSRF token is refused specifically for the CSRF reason,
// proving CSRF is the last gate, not the first thing that happens to reject.
func TestSecureChecksOriginAndHostBeforeCSRF(t *testing.T) {
	h := secure(okHandler, Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})
	req := newReq(t, http.MethodPost, "/api/refresh")
	req.Host = testBoundAddr
	req.Header.Set("Origin", "http://127.0.0.1:7788")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != csrfMismatchMsg {
		t.Errorf("body = %q, want the CSRF reload message", got)
	}
}

// TestSecureAllowsAGenuinelyGoodRequestThrough is the control case: every
// gate satisfied must actually let the request reach the handler, and still
// carry the security headers.
func TestSecureAllowsAGenuinelyGoodRequestThrough(t *testing.T) {
	h := secure(okHandler, Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})
	req := newReq(t, http.MethodPost, "/api/refresh")
	req.Host = testBoundAddr
	req.Header.Set("Origin", "http://127.0.0.1:7788")
	req.Header.Set("X-Csrf-Token", "tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff header missing on a successful response: %q", got)
	}
}

// ---- HostOriginOnly (the -tcp wrap) ----

func TestHostOriginOnlyAllowsHeaderlessCLIShapedPost(t *testing.T) {
	h := HostOriginOnly(okHandler, testBoundAddr)
	req := newReq(t, http.MethodPost, "/api/approve") // no Origin, no CSRF header: how wt/agents talk
	req.Host = testBoundAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a headerless CLI-shaped POST (no CSRF layer on -tcp); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHostOriginOnlyRefusesBrowserShapedForeignOriginPost(t *testing.T) {
	h := HostOriginOnly(okHandler, testBoundAddr)
	req := newReq(t, http.MethodPost, "/api/approve")
	req.Host = testBoundAddr
	req.Header.Set("Origin", "http://evil.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a foreign-Origin POST over -tcp; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHostOriginOnlyRefusesWrongHost(t *testing.T) {
	h := HostOriginOnly(okHandler, testBoundAddr)
	req := newReq(t, http.MethodGet, "/api/version")
	req.Host = "evil.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// ---- NewCSRFToken ----

func TestNewCSRFTokenIsThirtyTwoHexChars(t *testing.T) {
	tok := NewCSRFToken()
	if len(tok) != 32 {
		t.Errorf("len(NewCSRFToken()) = %d, want 32", len(tok))
	}
	for _, c := range tok {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("NewCSRFToken() = %q contains a non-hex character %q", tok, c)
		}
	}
}

func TestNewCSRFTokenIsStableWithinAProcessButDiffersAcrossCalls(t *testing.T) {
	a, b := NewCSRFToken(), NewCSRFToken()
	if a == b {
		t.Error("two calls to NewCSRFToken produced the same value — crypto/rand should make a collision practically impossible")
	}
}
