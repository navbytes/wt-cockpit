package main

// `wt open` (P4-design.md §1.6): resolves wtd's web listener address via
// /api/status and launches a browser at the worktree's reading-room URL.
// $BROWSER-first is what makes this scriptably testable end to end.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- browserCommand (pure selection logic) ----

func TestBrowserCommandPrefersBROWSEREnvOverPlatform(t *testing.T) {
	if got := browserCommand("my-browser", "darwin"); got != "my-browser" {
		t.Errorf("browserCommand = %q, want the $BROWSER value to win", got)
	}
}

func TestBrowserCommandFallsBackToOpenOnDarwin(t *testing.T) {
	if got := browserCommand("", "darwin"); got != "open" {
		t.Errorf("browserCommand = %q, want %q", got, "open")
	}
}

func TestBrowserCommandFallsBackToXdgOpenOnLinux(t *testing.T) {
	if got := browserCommand("", "linux"); got != "xdg-open" {
		t.Errorf("browserCommand = %q, want %q", got, "xdg-open")
	}
}

func TestBrowserCommandEmptyOnUnrecognisedGOOS(t *testing.T) {
	if got := browserCommand("", "plan9"); got != "" {
		t.Errorf("browserCommand = %q, want empty (caller just prints the URL)", got)
	}
}

// ---- open() ----

func TestOpenErrorsWithHintWhenWebIsOff(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(statusPayload{WebAddr: ""})
	}))
	defer ts.Close()

	c := &client{http: ts.Client(), base: ts.URL}
	err := c.open("some-id")
	if err == nil {
		t.Fatal("expected an error when -web is off")
	}
	if !strings.Contains(err.Error(), "-web 127.0.0.1:7788") {
		t.Errorf("error = %q, want it to hint the -web flag", err.Error())
	}
}

func TestOpenErrorsWhenStatusUnreachable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close() // nothing listening: connection refused

	c := &client{http: &http.Client{}, base: url}
	if err := c.open("some-id"); err == nil {
		t.Error("expected an error when wtd is unreachable")
	}
}

// TestOpenLaunchesBrowserEnvAndPrintsTheExactRoomURL is the scriptable,
// end-to-end phase-acceptance case: BROWSER=echo makes openBrowser run
// `echo <url>`, whose stdout is wired to wt's own — captureStdout blocks
// until every writer of the pipe (including the forked `echo`, even though
// wt itself doesn't wait for it) has closed it, which happens the instant
// `echo` exits, so this is deterministic without any sleep/poll.
func TestOpenLaunchesBrowserEnvAndPrintsTheExactRoomURL(t *testing.T) {
	t.Setenv("BROWSER", "echo")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(statusPayload{WebAddr: "127.0.0.1:7788"})
	}))
	defer ts.Close()

	c := &client{http: ts.Client(), base: ts.URL}
	out := captureStdout(t, func() {
		if err := c.open("abc123"); err != nil {
			t.Fatal(err)
		}
	})
	want := "http://127.0.0.1:7788/wt/abc123"
	if !strings.Contains(out, want) {
		t.Errorf("stdout = %q, want it to contain %q", out, want)
	}
}
