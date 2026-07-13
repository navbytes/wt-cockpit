package main

// Independent verification pass over cmd/wt, filling gaps left after the
// initial P2 handoff: a malformed-JSON /api/version response, forward
// compatibility with an SSE event type the parser has never heard of, and
// that `wt status --json` actually emits round-trippable JSON. Deliberately
// does not repeat coverage already in main_test.go (the handshake's
// connection-refused/404/mismatch/match cases, the hello-frame skip itself).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// ---- handshake: malformed daemon response ----

// TestCheckVersionReturnsErrorOnMalformedJSON: a daemon that answers
// /api/version with a 200 but a body that isn't valid JSON at all (as opposed
// to valid JSON with a wrong/missing "protocol" field) must not be treated as
// a successful handshake.
func TestCheckVersionReturnsErrorOnMalformedJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{not valid json")
	}))
	defer ts.Close()

	c := &client{http: ts.Client(), base: ts.URL}
	err := c.checkVersion()
	if err == nil {
		t.Fatal("expected an error when wtd's /api/version body is not valid JSON")
	}
	if !strings.Contains(err.Error(), "/api/version") {
		t.Errorf("checkVersion() error = %q, want it to mention /api/version (the existing decode-error wrap)", err.Error())
	}
}

// ---- SSE parser forward-compat ----

// TestShouldRenderSSELineRendersOnUnknownFutureEventType pins forward
// compatibility: an event name wt's parser has never heard of (e.g. one a
// future wtd version introduces) must not be treated specially. Only the
// literal "hello" frame is ever suppressed — any other named event's data
// line must still render normally, and the blank line ending its frame must
// still reset the event name for whatever comes next.
func TestShouldRenderSSELineRendersOnUnknownFutureEventType(t *testing.T) {
	var event string
	if got := shouldRenderSSELine("event: futuristic", &event); got {
		t.Error("an event: line itself should never trigger a render")
	}
	if got := shouldRenderSSELine(`data: {"type":"something-from-the-future"}`, &event); !got {
		t.Error("a data: line inside an unrecognised (non-hello) named event must still render — forward-compat")
	}
	if got := shouldRenderSSELine("", &event); got {
		t.Error("a blank line itself should never trigger a render")
	}
	if got := shouldRenderSSELine(`data: {"type":"worktree.upserted"}`, &event); !got {
		t.Error("after the blank line resets the event name, a later data: line must render normally")
	}
}

// ---- wt status --json ----

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. Needed because c.status/renderStatus print
// straight to os.Stdout rather than an injectable io.Writer.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestStatusJSONOutputRoundTripsThroughUnmarshal pins `wt status --json`'s
// actual contract: whatever it prints must be valid JSON that unmarshals back
// into the exact same statusPayload the daemon sent — not just "looks like
// JSON" but is byte-for-byte semantically identical after a round trip.
func TestStatusJSONOutputRoundTripsThroughUnmarshal(t *testing.T) {
	want := statusPayload{
		Version: "v0.2.0-test", Protocol: 1, UptimeSeconds: 12.5,
		SocketPath: "/tmp/x.sock", WatcherMode: "fsnotify", Roots: []string{"/a", "/b"},
		StatePath: "/tmp/state.json", RepoCount: 2, WorktreeCount: 3, ReviewedFiles: 1, TotalFiles: 5,
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer ts.Close()

	c := &client{http: ts.Client(), base: ts.URL}

	out := captureStdout(t, func() {
		if err := c.status(true); err != nil {
			t.Fatal(err)
		}
	})

	var got statusPayload
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("wt status --json output did not round-trip through json.Unmarshal: %v\noutput:\n%s", err, out)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-tripped status = %+v, want %+v", got, want)
	}
}

// ---- wt status human output ----

// TestRenderStatusShowsPidAndOmitsFilterLineWhenUnset pins Item A/B's status
// additions: pid always renders, but the "filter:" line only appears when a
// repo discovery filter is actually configured — an unfiltered daemon's
// status output must look exactly as it did before these fields existed.
func TestRenderStatusShowsPidAndOmitsFilterLineWhenUnset(t *testing.T) {
	out := captureStdout(t, func() {
		renderStatus(statusPayload{Version: "v1", Pid: 4242, Roots: []string{"/a"}})
	})
	if !strings.Contains(out, "4242") {
		t.Errorf("renderStatus output = %q, want it to show the pid", out)
	}
	if strings.Contains(out, "filter") {
		t.Errorf("renderStatus output = %q, want no filter line when include/exclude are both unset", out)
	}
}

// TestRenderStatusShowsFilterLineWhenConfigured is the filter line's
// positive case: either list being non-empty must show the line, with the
// unset half rendered as "(none)" rather than blank.
func TestRenderStatusShowsFilterLineWhenConfigured(t *testing.T) {
	out := captureStdout(t, func() {
		renderStatus(statusPayload{Version: "v1", Roots: []string{"/a"}, ExcludeRepos: []string{"archive-*"}})
	})
	if !strings.Contains(out, "filter") {
		t.Errorf("renderStatus output = %q, want a filter line when exclude_repos is set", out)
	}
	if !strings.Contains(out, "archive-*") {
		t.Errorf("renderStatus output = %q, want it to name the exclude pattern", out)
	}
	if !strings.Contains(out, "include=(none)") {
		t.Errorf("renderStatus output = %q, want the unset half rendered as (none)", out)
	}
}
