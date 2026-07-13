package main

// Item B's graceful-exit guarantee, end to end against the real binary: wtd
// already wires signal.NotifyContext(SIGINT, SIGTERM) (main.go) and falls off
// the end of main() on ctx.Done(), which should already exit 0 and remove its
// Unix socket — this pins that today's behavior actually holds, since it's
// load-bearing for launchd KeepAlive SuccessfulExit=false (a clean SIGTERM
// exit must not look like a crash, or launchd restarts a daemon that was
// deliberately stopped). wtdBin/requireWtdBin come from integration_test.go;
// waitForUnixSocket from notify_smoke_test.go (same package, per this repo's
// "test helpers aren't importable across packages" convention).

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestWtdSIGTERMExitsZeroAndRemovesSocket(t *testing.T) {
	bin := requireWtdBin(t)

	// Short, non-descriptive dir for the socket path: sockaddr_un.sun_path is
	// ~104 bytes on macOS, and a t.TempDir()-style path (which embeds the
	// full test name) reliably blows past that — see cmd/wt/integration_test.go's
	// shortSocketDir for the same footgun/fix.
	sockDir, err := os.MkdirTemp("", "wtd-sigterm-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "wtd.sock")

	root := t.TempDir()
	statePath := filepath.Join(root, "state.json")

	cmd := exec.Command(bin, "-root", root, "-socket", sockPath, "-state", statePath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting wtd: %v", err)
	}
	waitForUnixSocket(t, sockPath, 5*time.Second)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signaling wtd: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case waitErr := <-done:
		if waitErr != nil {
			t.Fatalf("wtd exit after SIGTERM = %v, want a clean exit 0", waitErr)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("wtd did not exit within 5s of SIGTERM")
	}

	if _, statErr := os.Stat(sockPath); !os.IsNotExist(statErr) {
		t.Errorf("socket file %s still exists after wtd exited (stat err=%v)", sockPath, statErr)
	}
}
