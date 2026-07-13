package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// ---- handshake (checkVersion) ----

// TestCheckVersionReturnsDaemonNotRunningErrorOnConnectionRefused mirrors the
// existing get()/post() "cannot reach wtd (is it running?)" wrapping: closing
// the httptest server before the request leaves nothing listening at its URL,
// which is the standard way to provoke a connection-refused error in tests.
func TestCheckVersionReturnsDaemonNotRunningErrorOnConnectionRefused(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close()

	c := &client{http: &http.Client{Timeout: 2 * time.Second}, base: url}
	err := c.checkVersion()
	if err == nil || !strings.Contains(err.Error(), "is it running") {
		t.Errorf("checkVersion() = %v, want an error in the existing daemon-not-running style", err)
	}
}

// TestCheckVersionReturns404FriendlyErrorForV01Daemon: a wtd predating the
// handshake has no /api/version route at all, so the mux/router would 404.
func TestCheckVersionReturns404FriendlyErrorForV01Daemon(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := &client{http: ts.Client(), base: ts.URL}
	err := c.checkVersion()
	if err == nil || !strings.Contains(err.Error(), "v0.1") {
		t.Errorf("checkVersion() = %v, want an error mentioning wtd is v0.1 (no handshake)", err)
	}
}

// TestCheckVersionReturnsMismatchError: a protocol version that disagrees
// with model.ProtocolVersion must hard-fail with an actionable message.
func TestCheckVersionReturnsMismatchError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"protocol": model.ProtocolVersion + 1, "version": "x", "goVersion": "go1.24",
		})
	}))
	defer ts.Close()

	c := &client{http: ts.Client(), base: ts.URL}
	err := c.checkVersion()
	if err == nil || !strings.Contains(err.Error(), "protocol mismatch") {
		t.Errorf("checkVersion() = %v, want a protocol mismatch error", err)
	}
}

// TestCheckVersionSucceedsOnMatchingProtocol is the control case proving the
// two error-path tests above are distinguishing a real check, not something
// that always fails.
func TestCheckVersionSucceedsOnMatchingProtocol(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"protocol": model.ProtocolVersion, "version": "x", "goVersion": "go1.24",
		})
	}))
	defer ts.Close()

	c := &client{http: ts.Client(), base: ts.URL}
	if err := c.checkVersion(); err != nil {
		t.Errorf("checkVersion() = %v, want nil for a matching protocol", err)
	}
}

// ---- SSE parser tolerance (shouldRenderSSELine) ----

func TestShouldRenderSSELineSkipsHelloFrame(t *testing.T) {
	var event string
	if got := shouldRenderSSELine("event: hello", &event); got {
		t.Error("an event: line itself should never trigger a render")
	}
	if got := shouldRenderSSELine(`data: {"protocol":1,"version":"dev","goVersion":"go1.24"}`, &event); got {
		t.Error("a data: line inside a hello frame must not trigger a render")
	}
}

func TestShouldRenderSSELineRendersUnnamedDataLines(t *testing.T) {
	var event string
	if got := shouldRenderSSELine(`data: {"type":"snapshot"}`, &event); !got {
		t.Error("a data: line with no preceding event: line must trigger a render (existing snapshot/worktree events)")
	}
}

func TestShouldRenderSSELineResetsEventNameAfterBlankLine(t *testing.T) {
	event := "hello"
	shouldRenderSSELine("", &event) // blank line ends the hello frame
	if got := shouldRenderSSELine(`data: {"type":"worktree.upserted"}`, &event); !got {
		t.Error("after the blank line ending a hello frame, a later unnamed data: line must render normally")
	}
}

// TestShouldRenderSSELineFullStreamSequence exercises a realistic sequence:
// hello frame, then the existing unnamed snapshot/worktree events, matching
// exactly what cmd/wtd's handleEvents emits.
func TestShouldRenderSSELineFullStreamSequence(t *testing.T) {
	lines := []string{
		"event: hello",
		`data: {"protocol":1,"version":"dev","goVersion":"go1.24"}`,
		"",
		`data: {"type":"snapshot"}`,
		"",
		`data: {"type":"worktree.upserted"}`,
		"",
	}
	want := []bool{false, false, false, true, false, true, false}

	var event string
	for i, line := range lines {
		if got := shouldRenderSSELine(line, &event); got != want[i] {
			t.Errorf("line %d (%q): shouldRenderSSELine = %v, want %v", i, line, got, want[i])
		}
	}
}

// ---- -version flag ----

// TestPrintVersionPrintsInjectedVersionString tests the -version flag's
// actual behaviour (the function main calls), not ldflags — go test can't
// exercise -ldflags -X, so this pins that whatever ends up in the package-
// level `version` var is what gets printed.
func TestPrintVersionPrintsInjectedVersionString(t *testing.T) {
	old := version
	version = "v0.2.0-test"
	defer func() { version = old }()

	var buf bytes.Buffer
	printVersion(&buf)

	if got := buf.String(); !strings.Contains(got, "v0.2.0-test") {
		t.Errorf("printVersion output = %q, want it to contain the injected version %q", got, "v0.2.0-test")
	}
}

// ---- bare `wt` TTY-default dispatch (P3-design.md §2.1, WP4) ----

// TestDefaultCommandTTYRunsTUI pins the routing decision at the function
// level (faking the "is a TTY" input, not a real terminal): a TTY makes bare
// `wt` the daily-driver full-screen TUI.
func TestDefaultCommandTTYRunsTUI(t *testing.T) {
	if got := defaultCommand(true); got != "tui" {
		t.Errorf("defaultCommand(true) = %q, want %q", got, "tui")
	}
}

// TestDefaultCommandNonTTYRunsLs is the other half: a pipe, redirect, or any
// other non-terminal stdout keeps today's `ls` text output so `wt | grep`
// and scripts never break.
func TestDefaultCommandNonTTYRunsLs(t *testing.T) {
	if got := defaultCommand(false); got != "ls" {
		t.Errorf("defaultCommand(false) = %q, want %q", got, "ls")
	}
}

// TestDefaultCommandIgnoresStdinEntirelyEvenWhenRedirected pins the
// "cron-ish edge" from P3-design.md §2.1: stdout TTY-ness is bare `wt`'s
// *only* signal ("if stdout is a TTY -> tui"), with no stdin carve-out
// anywhere in the design or in isTerminal/defaultCommand's signatures. This
// makes the intended behavior for a stdout-TTY-but-stdin-non-TTY combination
// (e.g. `wt </dev/null` at a real terminal, or any wrapper that reattaches a
// tty to stdout while redirecting stdin) explicit and pinned: it still opens
// the TUI, exactly like an ordinary interactive invocation, because
// defaultCommand structurally has no way to see stdin at all -- it takes a
// single bool. Redirecting os.Stdin here is a belt-and-suspenders way of
// making that structural fact concrete rather than merely asserted.
func TestDefaultCommandIgnoresStdinEntirelyEvenWhenRedirected(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	defer devNull.Close()
	savedStdin := os.Stdin
	os.Stdin = devNull
	defer func() { os.Stdin = savedStdin }()

	if got := defaultCommand(true); got != "tui" {
		t.Errorf("defaultCommand(true) with a non-TTY stdin = %q, want %q (stdout alone decides; stdin is never consulted)", got, "tui")
	}
}
