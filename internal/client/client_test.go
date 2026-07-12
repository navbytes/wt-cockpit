package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// testClient wires a Client directly onto an httptest.Server's default TCP
// transport, bypassing New/its unix-socket dialer — the same layering
// cmd/wt's own tests use (main_test.go's `&client{http: ts.Client(), base:
// ts.URL}`): fast, and the transport itself is exercised separately by
// TestNewDialsOverTheRealUnixSocket below.
func testClient(ts *httptest.Server) *Client {
	return &Client{http: ts.Client(), sse: ts.Client(), base: ts.URL}
}

// ---- Version ----

func TestVersionReturnsProtocolAndVersion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 7, "version": "v9.9.9", "goVersion": "go1.24"})
	}))
	defer ts.Close()

	protocol, version, err := testClient(ts).Version(context.Background())
	if err != nil {
		t.Fatalf("Version() error = %v, want nil", err)
	}
	if protocol != 7 || version != "v9.9.9" {
		t.Errorf("Version() = (%d, %q), want (7, %q)", protocol, version, "v9.9.9")
	}
}

// TestVersionReturns404FriendlyErrorForPreHandshakeDaemon: a wtd predating
// the handshake has no /api/version route at all (404), which must not be
// treated as "protocol 0" — it's a distinct, named condition.
func TestVersionReturns404FriendlyErrorForPreHandshakeDaemon(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()

	_, _, err := testClient(ts).Version(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pre-v0.2") {
		t.Errorf("Version() error = %v, want it to mention the daemon predates the handshake", err)
	}
}

func TestVersionReturnsErrorOnMalformedJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{not valid json")
	}))
	defer ts.Close()

	_, _, err := testClient(ts).Version(context.Background())
	if err == nil || !strings.Contains(err.Error(), "/api/version") {
		t.Errorf("Version() error = %v, want it to mention /api/version", err)
	}
}

// TestVersionWrapsConnectionRefusedAsUnreachableError pins the error-shaping
// contract every frontend relies on: a dead daemon must be distinguishable
// (via errors.As) from "daemon answered but refused", not just stringly.
// Mirrors cmd/wt's own TestCheckVersionReturnsDaemonNotRunningErrorOnConnectionRefused:
// close the server first so nothing listens at its URL.
func TestVersionWrapsConnectionRefusedAsUnreachableError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close()

	c := &Client{http: &http.Client{Timeout: 2 * time.Second}, sse: &http.Client{Timeout: 2 * time.Second}, base: url}
	_, _, err := c.Version(context.Background())
	var unreachable *UnreachableError
	if !errors.As(err, &unreachable) {
		t.Errorf("Version() error = %v (%T), want *UnreachableError", err, err)
	}
	if err == nil || !strings.Contains(err.Error(), "is it running") {
		t.Errorf("Version() error = %v, want it in the existing daemon-not-running voice", err)
	}
}

// ---- Worktrees / Diff ----

func TestWorktreesDecodesList(t *testing.T) {
	want := []model.Worktree{{ID: "abc123", Repo: "api", Name: "feature-x"}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/worktrees" {
			t.Errorf("path = %q, want /api/worktrees", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer ts.Close()

	got, err := testClient(ts).Worktrees(context.Background())
	if err != nil {
		t.Fatalf("Worktrees() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "abc123" || got[0].Name != "feature-x" {
		t.Errorf("Worktrees() = %+v, want %+v", got, want)
	}
}

func TestDiffDecodesStructuredDiffAndPassesIDThrough(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("id")
		_ = json.NewEncoder(w).Encode(model.Diff{WorktreeID: gotQuery, Base: "main"})
	}))
	defer ts.Close()

	d, err := testClient(ts).Diff(context.Background(), "wt-42")
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if gotQuery != "wt-42" {
		t.Errorf("server saw id=%q, want wt-42", gotQuery)
	}
	if d.WorktreeID != "wt-42" || d.Base != "main" {
		t.Errorf("Diff() = %+v", d)
	}
}

// TestDiffReturnsBodyVerbatimOnError: unlike a plain "wtd returned 404 Not
// Found", the daemon's actual reason (e.g. "unknown worktree id") is what a
// TUI error card / CLI message should show.
func TestDiffReturnsBodyVerbatimOnError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown worktree id", http.StatusNotFound)
	}))
	defer ts.Close()

	_, err := testClient(ts).Diff(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "unknown worktree id") {
		t.Errorf("Diff() error = %v, want the daemon's body verbatim", err)
	}
}

// ---- SetReviewed ----

func TestSetReviewedSucceeds(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer ts.Close()

	err := testClient(ts).SetReviewed(context.Background(), "wt-1", "a.go", true, "hash1")
	if err != nil {
		t.Fatalf("SetReviewed() error = %v", err)
	}
	if gotBody["id"] != "wt-1" || gotBody["file"] != "a.go" || gotBody["reviewed"] != true || gotBody["hash"] != "hash1" {
		t.Errorf("request body = %+v, want id/file/reviewed/hash to round-trip", gotBody)
	}
}

// TestSetReviewedReturnsConflictErrorOn409WithBodyVerbatim pins the P1
// conflict contract this client must carry forward: the file changed since
// the caller last viewed it, surfaced as a typed error the TUI can detect
// with errors.As (to trigger the optimistic-toggle revert), body verbatim.
func TestSetReviewedReturnsConflictErrorOn409WithBodyVerbatim(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "file changed since it was viewed: stale hash", http.StatusConflict)
	}))
	defer ts.Close()

	err := testClient(ts).SetReviewed(context.Background(), "wt-1", "a.go", true, "stale")
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("SetReviewed() error = %v (%T), want *ConflictError", err, err)
	}
	if !strings.Contains(conflict.Msg, "file changed since it was viewed") {
		t.Errorf("ConflictError.Msg = %q, want the daemon's body verbatim", conflict.Msg)
	}
}

func TestSetReviewedReturnsPlainErrorOnOtherNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "file not found in current diff", http.StatusNotFound)
	}))
	defer ts.Close()

	err := testClient(ts).SetReviewed(context.Background(), "wt-1", "missing.go", true, "")
	var conflict *ConflictError
	if errors.As(err, &conflict) {
		t.Errorf("SetReviewed() error = %v, want a plain error (not ConflictError) for a 404", err)
	}
	if err == nil || !strings.Contains(err.Error(), "file not found in current diff") {
		t.Errorf("SetReviewed() error = %v, want the daemon's body verbatim", err)
	}
}

// ---- Approve ----

func TestApproveSucceedsAndDecodesResult(t *testing.T) {
	want := model.ApproveResult{WorktreeID: "wt-1", Merged: "feature", Into: "main", Removed: "/path/wt-1"}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer ts.Close()

	got, err := testClient(ts).Approve(context.Background(), "wt-1")
	if err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if got != want {
		t.Errorf("Approve() = %+v, want %+v", got, want)
	}
}

// TestApproveReturnsGateErrorOn409WithBodyVerbatim pins the approve-gate
// contract: a refused gate is a typed error carrying the daemon's exact
// message (§1.4's approve-confirm modal shows it verbatim, red).
func TestApproveReturnsGateErrorOn409WithBodyVerbatim(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "cannot approve: 1 of 3 files not yet reviewed", http.StatusConflict)
	}))
	defer ts.Close()

	_, err := testClient(ts).Approve(context.Background(), "wt-1")
	var gate *GateError
	if !errors.As(err, &gate) {
		t.Fatalf("Approve() error = %v (%T), want *GateError", err, err)
	}
	if !strings.Contains(gate.Msg, "1 of 3 files not yet reviewed") {
		t.Errorf("GateError.Msg = %q, want the daemon's body verbatim", gate.Msg)
	}
}

// ---- Refresh ----

func TestRefreshSucceeds(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer ts.Close()

	if err := testClient(ts).Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if !called {
		t.Error("Refresh() never hit the server")
	}
}

func TestRefreshReturnsErrorOnNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	if err := testClient(ts).Refresh(context.Background()); err == nil {
		t.Error("Refresh() = nil error, want the 500 surfaced")
	}
}

// ---- Events (SSE) ----

// sseServer builds an httptest.Server that streams the given raw SSE frames
// (each already newline-terminated, "event:"/"data:" lines as wtd itself
// writes them) then closes the connection — mirroring cmd/wtd's handleEvents
// shape (hello, then snapshot, then live events) without needing a real
// engine.
func sseServer(t *testing.T, frames ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprint(w, f)
			flusher.Flush()
		}
	}))
}

// drainEvents collects exactly want events. It deliberately never selects on
// errs in the same case as events: Events() sends into a *buffered* errs
// channel, so once the goroutine is done, both events (with everything
// already queued) and errs can be simultaneously ready — select has no
// preference between ready cases, so racing them risks returning early with
// events still sitting unread in the channel.
func drainEvents(t *testing.T, events <-chan model.Event, errs <-chan error, want int) ([]model.Event, error) {
	t.Helper()
	var got []model.Event
	timeout := time.After(5 * time.Second)
	for len(got) < want {
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatalf("events channel closed early: got %d/%d events %+v", len(got), want, got)
			}
			got = append(got, e)
		case <-timeout:
			t.Fatalf("timed out waiting for SSE events: got %d/%d %+v", len(got), want, got)
		}
	}
	return got, <-errs
}

func TestEventsSkipsHelloFrameAndDeliversSnapshotAndWorktreeEvents(t *testing.T) {
	ts := sseServer(t,
		"event: hello\ndata: {\"protocol\":1,\"version\":\"dev\",\"goVersion\":\"go1.24\"}\n\n",
		"data: {\"type\":\"snapshot\"}\n\n",
		"data: {\"type\":\"worktree.upserted\",\"id\":\"wt-1\"}\n\n",
	)
	defer ts.Close()

	events, errs := testClient(ts).Events(context.Background())
	got, _ := drainEvents(t, events, errs, 2)

	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (hello must never be delivered as an event): %+v", len(got), got)
	}
	if got[0].Type != "snapshot" {
		t.Errorf("event[0].Type = %q, want snapshot", got[0].Type)
	}
	if got[1].Type != model.EventWorktreeUpserted || got[1].ID != "wt-1" {
		t.Errorf("event[1] = %+v, want worktree.upserted/wt-1", got[1])
	}
}

// TestEventsForwardCompatWithUnknownNamedEvent pins the same forward-compat
// contract cmd/wt's shouldRenderSSELine tests pin for the CLI: only the
// literal "hello" frame is ever suppressed.
func TestEventsForwardCompatWithUnknownNamedEvent(t *testing.T) {
	ts := sseServer(t, "event: futuristic\ndata: {\"type\":\"something-new\"}\n\n")
	defer ts.Close()

	events, errs := testClient(ts).Events(context.Background())
	got, _ := drainEvents(t, events, errs, 1)
	if len(got) != 1 || got[0].Type != "something-new" {
		t.Errorf("got %+v, want the unrecognised named event still delivered", got)
	}
}

// TestEventsClosesChannelsWithNilErrorOnCleanServerClose: when the server
// just ends the response (no error), the terminal error must be nil, not a
// spurious failure that would make the caller's reconnect loop log noise on
// every ordinary daemon-restart-driven disconnect.
func TestEventsClosesChannelsWithNilErrorOnCleanServerClose(t *testing.T) {
	ts := sseServer(t, "data: {\"type\":\"snapshot\"}\n\n")
	defer ts.Close()

	events, errs := testClient(ts).Events(context.Background())
	_, err := drainEvents(t, events, errs, 1)
	if err != nil {
		t.Errorf("terminal error = %v, want nil on a clean server-side close", err)
	}
	if _, ok := <-events; ok {
		t.Error("events channel should be closed after the terminal error")
	}
}

func TestEventsSendsUnreachableErrorWhenConnectionRefused(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts.Close() // nothing listening

	c := testClient(ts)
	events, errs := c.Events(context.Background())
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("expected no events when the daemon is unreachable")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for events channel to close")
	}
	err := <-errs
	var unreachable *UnreachableError
	if !errors.As(err, &unreachable) {
		t.Errorf("Events() error = %v (%T), want *UnreachableError", err, err)
	}
}

// TestEventsRespectsContextCancellation ensures the goroutine actually exits
// (both channels close) when the caller cancels, rather than leaking.
func TestEventsRespectsContextCancellation(t *testing.T) {
	ts := sseServer(t) // no frames; would otherwise hang open
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	events, errs := testClient(ts).Events(ctx)
	cancel()

	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("expected the events channel to close after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cancellation to close the events channel")
	}
	<-errs // drain; value doesn't matter (context.Canceled or nil race is fine)
}

// ---- New: the real Unix-socket transport ----

// shortSocketDir returns a freshly created temp directory suitable for a
// Unix socket path (short, fixed prefix — sockaddr_un.sun_path is ~104 bytes
// on macOS, and t.TempDir()'s descriptive-name paths reliably exceed that).
// Mirrors cmd/wt/integration_test.go's helper of the same name/rationale.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wtclientsock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestNewDialsOverTheRealUnixSocket exercises New(socket) end to end — the
// one test in this file that doesn't take the httptest.Server shortcut of
// bypassing the dialer — proving the Unix-socket Transport wiring itself
// (not just the JSON shaping already covered above) actually works.
func TestNewDialsOverTheRealUnixSocket(t *testing.T) {
	sockPath := filepath.Join(shortSocketDir(t), "wtd.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 3, "version": "sock-test", "goVersion": "go1.24"})
	})}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	c := New(sockPath)
	protocol, version, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version() over a real unix socket: %v", err)
	}
	if protocol != 3 || version != "sock-test" {
		t.Errorf("Version() = (%d, %q), want (3, %q)", protocol, version, "sock-test")
	}
}

// ---- SocketFromEnv ----

func TestSocketFromEnvUsesWTDSocketWhenSet(t *testing.T) {
	t.Setenv("WTD_SOCKET", "/custom/path.sock")
	if got := SocketFromEnv(); got != "/custom/path.sock" {
		t.Errorf("SocketFromEnv() = %q, want /custom/path.sock", got)
	}
}

func TestSocketFromEnvDefaultsUnderHomeWtcockpitDir(t *testing.T) {
	t.Setenv("WTD_SOCKET", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := filepath.Join(home, ".wtcockpit", "wtd.sock")
	if got := SocketFromEnv(); got != want {
		t.Errorf("SocketFromEnv() = %q, want %q", got, want)
	}
}
