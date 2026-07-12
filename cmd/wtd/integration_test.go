package main

// Process-level tests for behavior main() only enforces via os.Exit, which
// can't be observed by calling functions in-process (see the "strict
// validation" item in the P2 probe: -log-format/-log-level typos, including
// ones sourced from the config file, must exit 1 with a naming message).
// These build the real wtd binary once (TestMain) and run it as a subprocess.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wtdBin is the path to a wtd binary built once for every test in this
// package, populated by TestMain. Empty if the build failed (e.g. no network
// access for a cold module cache in some sandboxed CI environment) — tests
// needing it skip cleanly via requireWtdBin rather than failing the whole run.
var wtdBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wtd-bin-*")
	if err == nil {
		bin := filepath.Join(dir, "wtd")
		if buildErr := exec.Command("go", "build", "-o", bin, ".").Run(); buildErr == nil {
			wtdBin = bin
		}
	}
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

func requireWtdBin(t *testing.T) string {
	t.Helper()
	if wtdBin == "" {
		t.Skip("wtd binary could not be built in this environment")
	}
	return wtdBin
}

// runWtd runs the built wtd binary with args and a 5s kill-switch (a
// validation regression that makes wtd hang instead of exiting must fail the
// test quickly, not the whole `go test` run), returning its exit error and
// captured stderr.
func runWtd(t *testing.T, args ...string) (error, string) {
	t.Helper()
	bin := requireWtdBin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return err, stderr.String()
}

// wantExitCode1 fails t unless err is an *exec.ExitError reporting exit code
// 1 — the exact code main() passes to os.Exit on these validation paths (not
// merely "nonzero", so a future change to e.g. os.Exit(2) is caught too).
func wantExitCode1(t *testing.T, err error, stderr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a nonzero exit, got success; stderr=%s", stderr)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an *exec.ExitError, got %v (stderr=%s)", err, stderr)
	}
	if ee.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1; stderr=%s", ee.ExitCode(), stderr)
	}
}

// TestWtdExitsOneOnInvalidLogFormatFlag pins the -log-format validation exit
// path end to end (not just reachable code, but the actual process exit code
// and message an operator sees).
func TestWtdExitsOneOnInvalidLogFormatFlag(t *testing.T) {
	dir := t.TempDir()
	err, stderr := runWtd(t,
		"-root", dir,
		"-socket", filepath.Join(dir, "wtd.sock"),
		"-state", filepath.Join(dir, "state.json"),
		"-log-format", "bogus",
	)
	wantExitCode1(t, err, stderr)
	if !strings.Contains(stderr, "invalid log-format") {
		t.Errorf("stderr = %q, want it to mention %q", stderr, "invalid log-format")
	}
	if !strings.Contains(stderr, "bogus") {
		t.Errorf("stderr = %q, want it to name the offending value %q", stderr, "bogus")
	}
}

// TestWtdExitsOneOnInvalidLogLevelFlag is the -log-level counterpart.
func TestWtdExitsOneOnInvalidLogLevelFlag(t *testing.T) {
	dir := t.TempDir()
	err, stderr := runWtd(t,
		"-root", dir,
		"-socket", filepath.Join(dir, "wtd.sock"),
		"-state", filepath.Join(dir, "state.json"),
		"-log-level", "bogus",
	)
	wantExitCode1(t, err, stderr)
	if !strings.Contains(stderr, "invalid log-level") {
		t.Errorf("stderr = %q, want it to mention %q", stderr, "invalid log-level")
	}
	if !strings.Contains(stderr, "bogus") {
		t.Errorf("stderr = %q, want it to name the offending value %q", stderr, "bogus")
	}
}

// TestWtdExitsOneOnInvalidLogLevelFromConfigFile pins that a config-file typo
// is refused by the *same* validation as the flag, not silently accepted:
// config.Load never validates log_level's value (it just parses the TOML
// key), so an invalid value only gets caught because mergeSetting feeds it
// into the identical switch main() uses for the flag.
func TestWtdExitsOneOnInvalidLogLevelFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("log_level = \"bogus\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err, stderr := runWtd(t,
		"-config", cfgPath,
		"-root", dir,
		"-socket", filepath.Join(dir, "wtd.sock"),
		"-state", filepath.Join(dir, "state.json"),
	)
	wantExitCode1(t, err, stderr)
	if !strings.Contains(stderr, "invalid log-level") {
		t.Errorf("stderr = %q, want it to mention %q", stderr, "invalid log-level")
	}
	if !strings.Contains(stderr, "bogus") {
		t.Errorf("stderr = %q, want it to name the offending config value %q", stderr, "bogus")
	}
}
