package main

// Live smoke test for WP2's desktop notifier: a REAL wtd subprocess, a stub
// notifier binary on PATH, against a real temp git repo that trips a danger
// guardrail rule after startup — end to end, no HTTP assertions needed (the
// stub's own argv log is the observable). wtdBin/requireWtdBin come from
// probe_test.go; testGit/testGitEnv from main_test.go (same package).

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// wantNotifierBinaryName mirrors internal/notify's unexported
// wantBinaryName (test helpers aren't importable across packages, per this
// repo's convention — see e.g. cmd/wtd/main_test.go's mustResolver/testGit).
func wantNotifierBinaryName(goos string) string {
	switch goos {
	case "darwin":
		return "osascript"
	case "linux":
		return "notify-send"
	default:
		return ""
	}
}

// writeStubNotifier writes an executable stub — named per GOOS's expected
// notifier binary — into dir that appends its argv to logPath: one element
// per line, followed by an "===END===" marker. Safe as plain shell text
// because every argv element the real notifier execs has already passed
// through notify.Sanitize, which strips raw newlines.
func writeStubNotifier(t *testing.T, dir, logPath string) {
	t.Helper()
	name := wantNotifierBinaryName(runtime.GOOS)
	if name == "" {
		t.Skipf("no notifier binary defined for GOOS %q", runtime.GOOS)
	}
	script := `#!/bin/sh
{
for a in "$@"; do
  printf '%s\n' "$a"
done
printf '===END===\n'
} >> "` + logPath + `"
exit 0
`
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// readStubInvocations parses logPath's line-per-element, "===END==="-
// delimited records into one []string per invocation.
func readStubInvocations(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	const end = "===END===\n"
	content := string(data)
	var out [][]string
	for {
		i := strings.Index(content, end)
		if i < 0 {
			break
		}
		chunk := content[:i]
		content = content[i+len(end):]
		var argv []string
		if chunk != "" {
			argv = strings.Split(chunk, "\n")
			if len(argv) > 0 && argv[len(argv)-1] == "" {
				argv = argv[:len(argv)-1]
			}
		}
		out = append(out, argv)
	}
	return out
}

// waitForUnixSocket polls until sockPath accepts a connection, bounded.
func waitForUnixSocket(t *testing.T, sockPath string, timeout time.Duration) {
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

// waitForInvocations polls logPath until it has at least want invocations,
// bounded, failing (with the wtd process's stderr for debugging) on timeout.
func waitForInvocations(t *testing.T, logPath string, want int, stderr *strings.Builder) [][]string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got := readStubInvocations(t, logPath)
		if len(got) >= want {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d notifier invocation(s), got %d\nwtd stderr:\n%s",
		want, len(readStubInvocations(t, logPath)), stderr.String())
	return nil
}

// TestLiveSmokeNotifierFiresOnDangerHitAndCoalescesABurst is WP2's headline
// DONE-WHEN check: a real wtd process, a stub notifier on PATH, a temp repo
// that trips a danger rule (touches-migrations) in TWO files at once (the
// "burst") after the daemon's first scan (so the cold-start gate doesn't
// swallow it) — the stub must receive exactly ONE invocation (the burst
// coalesced), naming the worktree and the rule's message, with no shell
// metacharacter or secret-shaped content in the argv.
func TestLiveSmokeNotifierFiresOnDangerHitAndCoalescesABurst(t *testing.T) {
	bin := requireWtdBin(t)

	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "add", ".")
	testGit(t, repo, "commit", "-q", "-m", "init")

	stubDir := t.TempDir()
	logPath := filepath.Join(stubDir, "argv.log")
	writeStubNotifier(t, stubDir, logPath)

	sockDir, err := os.MkdirTemp("", "wtd-notify-smoke-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "wtd.sock")
	statePath := filepath.Join(root, "state.json")

	cmd := exec.Command(bin,
		"-root", root,
		"-socket", sockPath,
		"-state", statePath,
		"-watch", "poll",
		"-interval", "150ms",
	)
	cmd.Env = append(os.Environ(), "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting wtd: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	waitForUnixSocket(t, sockPath, 5*time.Second)
	// Give the poller's immediate first scan (Poller.Run fires onChange("")
	// synchronously at startup) time to complete, so the cold-start gate
	// (firstScanDone) is open before the offending worktree is created —
	// otherwise the engine would evaluate it as part of the FIRST scan and
	// the resulting hits would be suppressed as "standing", never published
	// as guardrail.tripped at all.
	time.Sleep(300 * time.Millisecond)

	// The burst: a brand new worktree touching TWO files under migrations/ at
	// once trips the default "touches-migrations" (danger) rule twice in the
	// SAME refresh — two distinct (rule,file) keys published back-to-back,
	// which the notifier's 5s coalescing window must collapse into ONE
	// notification, not two.
	feature := filepath.Join(root, "repo-feature")
	testGit(t, repo, "worktree", "add", "-q", "-b", "feature", feature)
	if err := os.MkdirAll(filepath.Join(feature, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(feature, "migrations", "001_init.sql"), []byte("create table t();\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(feature, "migrations", "002_add_col.sql"), []byte("alter table t add c int;\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	invocations := waitForInvocations(t, logPath, 1, &stderr)

	// Give any (incorrect) second invocation a moment to show up before
	// asserting the count — proves the burst genuinely coalesced rather than
	// the test just having checked before the second one arrived.
	time.Sleep(500 * time.Millisecond)
	invocations = readStubInvocations(t, logPath)
	if len(invocations) != 1 {
		t.Fatalf("notifier invocations = %d, want exactly 1 (the two-file burst must coalesce): %+v\nwtd stderr:\n%s",
			len(invocations), invocations, stderr.String())
	}

	argv := invocations[0]
	joined := strings.Join(argv, "\x1f")
	// Title is "wt-cockpit — <repo.Name>/<branch>" = "wt-cockpit — repo/feature"
	// (repo.Name is the repo directory's basename; the worktree's Name is its
	// checked-out branch, "feature" — see engine.branchName), body is the
	// rule's message plus the "…and N more" suffix from the two-file burst.
	for _, want := range []string{"repo/feature", "touches database migrations", "…and 1 more"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notifier argv %q missing %q", argv, want)
		}
	}
	for _, bad := range []string{";", "$(", "`", "\n", "\x1b"} {
		if strings.Contains(joined, bad) {
			t.Errorf("notifier argv %q must not contain %q (shell metachar or escape byte)", argv, bad)
		}
	}
}
