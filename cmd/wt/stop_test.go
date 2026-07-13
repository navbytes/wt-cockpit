package main

// `wt stop` e2e tests (Item B): a real wtd subprocess on a private socket,
// stopped via the real wt binary, plus the "already stopped"/dead-socket
// path. wtBin/requireWtBin and shortSocketDir come from integration_test.go
// (same package, per this repo's "test helpers aren't importable across
// packages" convention).

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
	"strings"
	"testing"
	"time"
)

var (
	wtdBinForStopTests      string
	wtdBinForStopTestsTried bool
)

// requireWtdBinForStop builds the real wtd binary once (lazily — only the
// stop e2e test below needs a live daemon), skipping cleanly if the build
// fails, same convention as requireWtBin/requireWtdBin elsewhere. A second
// TestMain isn't an option (this package's own TestMain, in
// integration_test.go, already builds the wt binary), so building
// lazily-on-first-use is the seam instead.
func requireWtdBinForStop(t *testing.T) string {
	t.Helper()
	if !wtdBinForStopTestsTried {
		wtdBinForStopTestsTried = true
		dir, err := os.MkdirTemp("", "wtd-bin-for-wt-test-")
		if err == nil {
			bin := filepath.Join(dir, "wtd")
			if buildErr := exec.Command("go", "build", "-o", bin, "../wtd").Run(); buildErr == nil {
				wtdBinForStopTests = bin
			}
		}
	}
	if wtdBinForStopTests == "" {
		t.Skip("wtd binary could not be built in this environment")
	}
	return wtdBinForStopTests
}

// waitForSocket polls until sockPath accepts a connection, bounded — local
// equivalent of cmd/wtd's own waitForUnixSocket (test helpers aren't
// importable across packages).
func waitForSocket(t *testing.T, sockPath string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", sockPath); err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("wtd did not start listening on %s within %s", sockPath, timeout)
}

// TestWtStopStopsRealDaemonExitZero runs the real wt binary's `stop`
// subcommand against a real wtd subprocess: it must exit 0, print a message
// that the daemon stopped, and the daemon must actually finish exiting (its
// socket file removed) — not merely refuse the very next request.
func TestWtStopStopsRealDaemonExitZero(t *testing.T) {
	wtBin := requireWtBin(t)
	wtdBin := requireWtdBinForStop(t)

	sockDir := shortSocketDir(t)
	sockPath := filepath.Join(sockDir, "wtd.sock")
	root := t.TempDir()
	statePath := filepath.Join(root, "state.json")

	daemon := exec.Command(wtdBin, "-root", root, "-socket", sockPath, "-state", statePath)
	if err := daemon.Start(); err != nil {
		t.Fatalf("starting wtd: %v", err)
	}
	defer func() {
		_ = daemon.Process.Kill()
		_ = daemon.Wait()
	}()
	waitForSocket(t, sockPath, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, wtBin, "stop")
	cmd.Env = append(os.Environ(), "WTD_SOCKET="+sockPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("wt stop = %v, want exit 0; stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "stopped") {
		t.Errorf("stdout = %q, want it to mention the daemon stopped", stdout.String())
	}

	// wt stop's own poll loop only proves the socket stopped ACCEPTING
	// requests; confirm the daemon actually finished exiting (socket file
	// removed, per its SIGTERM shutdown path — see cmd/wtd/signal_test.go)
	// within a further bounded wait, rather than trusting that alone.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sockPath); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket file %s still exists after wt stop succeeded", sockPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestWtStopAgainstDeadSocketExitsOneWithNotRunningMessage pins the "already
// stopped" outcome: no daemon listening at all must exit 1 with the
// dedicated "wtd is not running" message (not checkVersion's generic "cannot
// reach wtd", which wt stop deliberately bypasses — see main's dispatch).
func TestWtStopAgainstDeadSocketExitsOneWithNotRunningMessage(t *testing.T) {
	bin := requireWtBin(t)
	sockPath := filepath.Join(shortSocketDir(t), "no-daemon.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "stop")
	cmd.Env = append(os.Environ(), "WTD_SOCKET="+sockPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	if err == nil {
		t.Fatalf("expected a nonzero exit when wtd is down, got success; stderr=%s", stderr.String())
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Errorf("exit = %v, want exit code 1; stderr=%s", err, stderr.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "wtd is not running") {
		t.Errorf("stderr = %q, want it to mention %q", got, "wtd is not running")
	}
	if !strings.Contains(got, sockPath) {
		t.Errorf("stderr = %q, want it to name the socket path %q", got, sockPath)
	}
}

// ---- stop() unit pieces (no live daemon needed) ----

// TestClientStopReturnsNotRunningMessageOnConnectionRefused mirrors
// TestCheckVersionReturnsDaemonNotRunningErrorOnConnectionRefused's own
// pattern (main_test.go): closing the httptest server before the request
// leaves nothing listening, provoking the same connection-refused path
// stop's own GET /api/status takes when wtd genuinely isn't running.
func TestClientStopReturnsNotRunningMessageOnConnectionRefused(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close()

	c := &client{http: &http.Client{Timeout: 2 * time.Second}, base: url}
	err := c.stop("/tmp/example.sock", "darwin")
	if err == nil || !strings.Contains(err.Error(), "wtd is not running") {
		t.Errorf("stop() = %v, want a \"wtd is not running\" error", err)
	}
	if err != nil && !strings.Contains(err.Error(), "/tmp/example.sock") {
		t.Errorf("stop() error = %v, want it to name the socket path", err)
	}
}

// TestClientStopOnWindowsReturnsGuidanceWithoutSignaling: goos="windows"
// must short-circuit before ever calling os.FindProcess/Signal (there's no
// deliverable SIGTERM there) — using an implausible pid proves it, since
// reaching that code would surface a *different* error ("cannot find
// process"/"cannot signal") instead of the windows guidance message.
func TestClientStopOnWindowsReturnsGuidanceWithoutSignaling(t *testing.T) {
	const fakePid = 999999999 // implausible on any real machine
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(statusPayload{Pid: fakePid})
	}))
	defer ts.Close()

	c := &client{http: &http.Client{Timeout: 2 * time.Second}, base: ts.URL}
	err := c.stop("/tmp/example.sock", "windows")
	want := windowsStopMessage(fakePid)
	if err == nil || err.Error() != want {
		t.Errorf("stop() on windows = %v, want %q", err, want)
	}
}

// TestWindowsStopMessageNamesPidAndGuidance pins the guidance text's content
// (brief's suggested wording: "stop wtd from the terminal/task manager;
// pid N") rather than just its non-emptiness.
func TestWindowsStopMessageNamesPidAndGuidance(t *testing.T) {
	got := windowsStopMessage(4242)
	for _, want := range []string{"4242", "Windows", "terminal", "task manager"} {
		if !strings.Contains(got, want) {
			t.Errorf("windowsStopMessage(4242) = %q, want it to contain %q", got, want)
		}
	}
}
