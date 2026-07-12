package main

// Process-level tests for behavior main() only enforces via os.Exit (fatal),
// which can't be observed by calling functions in-process: the full exit-1
// path when the daemon speaks a mismatched protocol, and a nonzero exit when
// the daemon isn't reachable at all. These build the real wt binary once
// (TestMain) and run it as a subprocess.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// wtBin is the path to a wt binary built once for every test in this
// package, populated by TestMain. Empty if the build failed — tests needing
// it skip cleanly via requireWtBin rather than failing the whole run.
var wtBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wt-bin-*")
	if err == nil {
		bin := filepath.Join(dir, "wt")
		if buildErr := exec.Command("go", "build", "-o", bin, ".").Run(); buildErr == nil {
			wtBin = bin
		}
	}
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

func requireWtBin(t *testing.T) string {
	t.Helper()
	if wtBin == "" {
		t.Skip("wt binary could not be built in this environment")
	}
	return wtBin
}

// shortSocketDir returns a freshly created temp directory suitable for a Unix
// socket path, with automatic cleanup. Unlike t.TempDir(), which embeds the
// (often long, descriptive) test name into the path, this uses a short fixed
// prefix: sockaddr_un.sun_path is ~104 bytes on macOS, and a t.TempDir()-style
// path combined with a descriptive test name reliably exceeds that — verified
// empirically, it fails bind with "invalid argument" instead of exercising
// the intended scenario at all.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wtsock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// unixSocketServer starts httptest handler h listening on a Unix socket at
// sockPath instead of httptest's default TCP loopback address — the only
// transport wt's client ever dials (see newClient/WTD_SOCKET), so exercising
// the real subprocess against a fake daemon requires a real Unix listener.
func unixSocketServer(t *testing.T, sockPath string, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(h)
	ts.Listener.Close()
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ts.Listener = l
	ts.Start()
	return ts
}

// TestWtExitsOneOnProtocolMismatchAndMentionsBothNumbers runs the actual wt
// binary against a fake daemon speaking protocol 999 and pins the full exit-1
// path end to end: not just that checkVersion() returns an error (already
// pinned in-process in main_test.go/probe_test.go), but that the real
// process exits with code 1 and prints a message naming both wt's own
// protocol number and the daemon's.
func TestWtExitsOneOnProtocolMismatchAndMentionsBothNumbers(t *testing.T) {
	bin := requireWtBin(t)
	sockPath := filepath.Join(shortSocketDir(t), "wtd.sock")
	ts := unixSocketServer(t, sockPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": 999, "version": "x", "goVersion": "go1.24"})
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "status")
	cmd.Env = append(os.Environ(), "WTD_SOCKET="+sockPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	if err == nil {
		t.Fatalf("expected a nonzero exit on protocol mismatch, got success; stderr=%s", stderr.String())
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Errorf("exit = %v, want exit code 1; stderr=%s", err, stderr.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "999") {
		t.Errorf("stderr = %q, want it to mention the daemon's protocol number 999", got)
	}
	if !strings.Contains(got, strconv.Itoa(model.ProtocolVersion)) {
		t.Errorf("stderr = %q, want it to mention wt's own protocol number %d", got, model.ProtocolVersion)
	}
}

// TestWtWithNoArgsAndNonTTYStdoutPrintsLsText is P3-design.md §5 phase-
// acceptance item 5 ("wt | cat still prints the ls radar, exit 0") and the
// other half of TestDefaultCommand*'s function-level seam-faking in
// main_test.go: cmd.Stdout below is a plain buffer (exec wires it through an
// OS pipe), so the real isTerminal() seam sees a genuine non-terminal fd —
// this exercises the production isatty check end to end, not a faked one.
func TestWtWithNoArgsAndNonTTYStdoutPrintsLsText(t *testing.T) {
	bin := requireWtBin(t)
	sockPath := filepath.Join(shortSocketDir(t), "wtd.sock")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": model.ProtocolVersion, "version": "x", "goVersion": "go1.24"})
	})
	mux.HandleFunc("/api/worktrees", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]model.Worktree{{ID: "a1", Repo: "api-server", Name: "feature-x", Base: "main"}})
	})
	ts := unixSocketServer(t, sockPath, mux)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin) // bare `wt`, no args
	cmd.Env = append(os.Environ(), "WTD_SOCKET="+sockPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bare wt with piped stdout = %v, want exit 0; stderr=%s", err, stderr.String())
	}

	got := stdout.String()
	if !strings.Contains(got, "wt cockpit") {
		t.Errorf("stdout = %q, want the ls radar header — bare wt with non-TTY stdout must behave exactly like `wt ls`", got)
	}
	if !strings.Contains(got, "feature-x") {
		t.Errorf("stdout = %q, want the fixture worktree name", got)
	}
	if strings.Contains(got, "\x1b[?1049h") {
		t.Errorf("stdout contains an alt-screen escape sequence; want plain ls text (never the TUI) when stdout isn't a terminal")
	}
}

// TestWtStatusAgainstDownDaemonExitsNonZero pins the CLI's observable failure
// mode when wtd isn't running at all: `wt status` must exit nonzero with an
// actionable stderr message, not hang or succeed silently. WTD_SOCKET points
// at a path nothing is listening on, guaranteeing connection-refused.
func TestWtStatusAgainstDownDaemonExitsNonZero(t *testing.T) {
	bin := requireWtBin(t)
	sockPath := filepath.Join(shortSocketDir(t), "no-daemon.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "status")
	cmd.Env = append(os.Environ(), "WTD_SOCKET="+sockPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	if err == nil {
		t.Fatalf("expected a nonzero exit when wtd is down, got success; stderr=%s", stderr.String())
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Errorf("exit = %v, want a nonzero exit code; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "wt: ") {
		t.Errorf("stderr = %q, want the wt: prefixed fatal message", stderr.String())
	}
}
