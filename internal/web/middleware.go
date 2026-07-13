package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
)

// cspHeader is the exact, frozen CSP for every HTML/fragment/API response on
// the web listener (P4-design.md §1.3): no inline script anywhere, nothing
// cross-origin, no framing.
const cspHeader = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// csrfMismatchMsg is the exact 403 body a missing/wrong X-Csrf-Token gets —
// app.js (WP4) reloads the page on this specific message when the daemon
// restarted mid-session and issued a new token.
const csrfMismatchMsg = "csrf token mismatch — reload the page"

// NewCSRFToken generates the process-lifetime CSRF token (§1.3): 16
// crypto/rand bytes, hex-encoded to 32 characters. Called once at daemon
// startup; the same value is embedded in every rendered page's
// <meta name="csrf-token"> and compared, constant-time, against every
// non-GET/HEAD web-listener request's X-Csrf-Token header.
func NewCSRFToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the OS's CSPRNG is broken: there is no
		// sane fallback for a security token, so panicking here (same
		// posture as e.g. google/uuid) is the right call.
		panic("web: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// secure wraps mux with the full browser-security stack (§1.3). Order is
// deliberate and tested: secHeaders is outermost so even a refused request's
// error response carries CSP/nosniff/etc; hostAllowed runs before
// originAllowed so an unrecognised Host is refused regardless of Origin (the
// DNS-rebinding defense must not depend on Origin being present at all);
// csrfRequired runs last, once a request is already known to be
// same-origin-shaped — it is the one layer HostOriginOnly (the -tcp wrap)
// never applies.
func secure(next http.Handler, cfg Config) http.Handler {
	return secHeaders(hostAllowed(cfg.BoundAddr, originAllowed(cfg.BoundAddr, csrfRequired(cfg.CSRFToken, next))))
}

// HostOriginOnly is the "free half" of the browser middleware (§1.1) applied
// to the pre-existing -tcp listener: Host + Origin/Sec-Fetch-Site validation
// only. No CSRF token and no security response headers — -tcp serves API
// only (no pages to protect with CSP), and non-browser clients (wt, agents)
// send no Origin header at all, so they pass through untouched exactly as
// before this middleware existed.
func HostOriginOnly(next http.Handler, boundAddr string) http.Handler {
	return hostAllowed(boundAddr, originAllowed(boundAddr, next))
}

// hostAllowed refuses any request whose Host header doesn't name the bound
// address, or "localhost"/"127.0.0.1"/"[::1]" on the same port (§1.3's
// DNS-rebinding defense — the load-bearing read protection). The check is a
// plain string comparison against r.Host: that is exactly why it holds even
// when the connecting socket really is loopback (evil.com resolved to
// 127.0.0.1 by a rebinding attacker) — the attack's whole premise is a Host
// header that lies about the origin, and this never trusts it.
func hostAllowed(boundAddr string, next http.Handler) http.Handler {
	allowed := allowedHosts(boundAddr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func allowedHosts(boundAddr string) map[string]bool {
	_, port, err := net.SplitHostPort(boundAddr)
	if err != nil {
		return map[string]bool{} // malformed bound address: refuse everything, fail closed
	}
	return map[string]bool{
		boundAddr:           true,
		"127.0.0.1:" + port: true,
		"localhost:" + port: true,
		"[::1]:" + port:     true,
	}
}

// originAllowed refuses a request whose Origin header is present but isn't
// one of this listener's own origins, and — independently — a request whose
// Sec-Fetch-Site header is present but not "same-origin"/"none" (§1.3, CSRF
// layers 1 and 3). Browsers attach Origin to every POST (including a
// cross-site form submission) and to a cross-origin fetch/EventSource, but
// not to a same-origin top-level navigation or a non-browser client's
// request, so an absent Origin passes through untouched — exactly what keeps
// wt/the TUI/agents (and -tcp today) working with zero Origin header at all.
func originAllowed(boundAddr string, next http.Handler) http.Handler {
	allowed := allowedOrigins(boundAddr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && !allowed[o] {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func allowedOrigins(boundAddr string) map[string]bool {
	_, port, err := net.SplitHostPort(boundAddr)
	if err != nil {
		return map[string]bool{}
	}
	return map[string]bool{
		"http://127.0.0.1:" + port: true,
		"http://localhost:" + port: true,
		"http://[::1]:" + port:     true,
	}
}

// csrfRequired refuses any non-GET/HEAD request that doesn't echo the
// process-lifetime token verbatim in X-Csrf-Token (constant-time compare;
// an empty configured token — which should never happen, New's caller
// always supplies one — refuses everything rather than comparing two empty
// strings equal). GET/HEAD are exempt: they're not state-changing, and the
// SSE endpoint (a GET) must stay reachable via EventSource, which cannot set
// custom headers at all.
func csrfRequired(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			got := r.Header.Get("X-Csrf-Token")
			if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, csrfMismatchMsg, http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// secHeaders sets the response headers every web-listener response carries
// (§1.3), before the request is even dispatched further — so a downstream
// Host/Origin/CSRF refusal's error response carries them too.
func secHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", cspHeader)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
