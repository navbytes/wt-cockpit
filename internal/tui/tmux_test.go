package tui

import (
	"errors"
	"strings"
	"testing"
)

// ---- parseTmuxPanes ----

func TestParseTmuxPanesParsesTabSeparatedLines(t *testing.T) {
	out := "%1\tsessA\t0\t/repo/a\n%2\tsessA\t1\t/repo/b\n"
	panes := parseTmuxPanes(out)
	if len(panes) != 2 {
		t.Fatalf("parsed %d panes, want 2: %+v", len(panes), panes)
	}
	want := tmuxPane{id: "%1", session: "sessA", window: "0", cwd: "/repo/a"}
	if panes[0] != want {
		t.Errorf("panes[0] = %+v, want %+v", panes[0], want)
	}
}

func TestParseTmuxPanesSkipsMalformedLines(t *testing.T) {
	out := "not-enough-fields\n%1\tsessA\t0\t/repo/a\n"
	panes := parseTmuxPanes(out)
	if len(panes) != 1 || panes[0].id != "%1" {
		t.Errorf("panes = %+v, want only the well-formed line", panes)
	}
}

func TestParseTmuxPanesEmptyOutputYieldsNoPanes(t *testing.T) {
	if panes := parseTmuxPanes(""); len(panes) != 0 {
		t.Errorf("panes = %+v, want none", panes)
	}
}

func TestParseTmuxPanesToleratesTrailingCROnEachLine(t *testing.T) {
	out := "%1\tsessA\t0\t/repo/a\r\n"
	panes := parseTmuxPanes(out)
	if len(panes) != 1 || panes[0].cwd != "/repo/a" {
		t.Errorf("panes = %+v, want cwd /repo/a with no stray \\r", panes)
	}
}

// ---- pickTmuxPane ----

func TestPickTmuxPaneExactMatchWins(t *testing.T) {
	panes := []tmuxPane{{id: "%1", cwd: "/repo"}, {id: "%2", cwd: "/repo/wt"}}
	got, ok := pickTmuxPane(panes, "/repo/wt")
	if !ok || got.id != "%2" {
		t.Errorf("pickTmuxPane = %+v, %v, want the exact match %%2", got, ok)
	}
}

// TestPickTmuxPaneMatchesSubdirAgentCdInto pins §2.7's "agents cd into
// subdirs" case: no pane sits exactly at the worktree root, but one is
// nested under it.
func TestPickTmuxPaneMatchesSubdirAgentCdInto(t *testing.T) {
	panes := []tmuxPane{{id: "%1", cwd: "/other"}, {id: "%2", cwd: "/repo/wt/internal/foo"}}
	got, ok := pickTmuxPane(panes, "/repo/wt")
	if !ok || got.id != "%2" {
		t.Errorf("pickTmuxPane = %+v, %v, want %%2 (nested under the worktree)", got, ok)
	}
}

// TestPickTmuxPanePrefersDeepestNestedMatch: with multiple panes nested
// under the same worktree, prefer the most specific (deepest) one.
func TestPickTmuxPanePrefersDeepestNestedMatch(t *testing.T) {
	panes := []tmuxPane{
		{id: "%1", cwd: "/repo/wt/sub"},
		{id: "%2", cwd: "/repo/wt/sub/deeper"},
	}
	got, ok := pickTmuxPane(panes, "/repo/wt")
	if !ok || got.id != "%2" {
		t.Errorf("pickTmuxPane = %+v, %v, want %%2 (the deepest nested pane)", got, ok)
	}
}

func TestPickTmuxPaneNoMatchForUnrelatedCwd(t *testing.T) {
	panes := []tmuxPane{{id: "%1", cwd: "/other"}}
	if _, ok := pickTmuxPane(panes, "/repo/wt"); ok {
		t.Error("pickTmuxPane ok = true, want false when no pane relates to the worktree")
	}
}

// TestPickTmuxPaneRejectsSimilarlyNamedSibling: "/repo/wt-other" must not
// false-positive as nested under "/repo/wt".
func TestPickTmuxPaneRejectsSimilarlyNamedSibling(t *testing.T) {
	panes := []tmuxPane{{id: "%1", cwd: "/repo/wt-other"}}
	if _, ok := pickTmuxPane(panes, "/repo/wt"); ok {
		t.Error("pickTmuxPane ok = true, want false — /repo/wt-other is a sibling, not a subdir")
	}
}

func TestPickTmuxPaneNoPanesReturnsNotOK(t *testing.T) {
	if _, ok := pickTmuxPane(nil, "/repo/wt"); ok {
		t.Error("pickTmuxPane ok = true for an empty pane list, want false")
	}
}

// ---- tmuxJump orchestration ----

func TestTmuxJumpNotInsideTmuxReturnsToastWithoutExec(t *testing.T) {
	msg := tmuxJump(false, func(args ...string) (string, error) {
		t.Fatal("must not exec tmux when not inside a tmux session")
		return "", nil
	}, "/some/path")
	if msg != "not inside tmux" {
		t.Errorf("tmuxJump = %q, want the not-inside-tmux toast", msg)
	}
}

// TestTmuxJumpListPanesArgvIsFixedNoShellInterpolation pins §2.7's "fixed
// argv, no shell interpolation of paths": the list-panes call's argv is
// always exactly this, regardless of the worktree path's contents.
func TestTmuxJumpListPanesArgvIsFixedNoShellInterpolation(t *testing.T) {
	var gotArgs []string
	tmuxJump(true, func(args ...string) (string, error) {
		if gotArgs == nil {
			gotArgs = args
		}
		return "", nil // no panes matched -> the "no match" path; args capture is what this test pins
	}, "/tmp/x; rm -rf /")
	want := []string{"list-panes", "-a", "-F", tmuxListPanesFormat}
	if len(gotArgs) != len(want) {
		t.Fatalf("argv = %v, want %v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, gotArgs[i], want[i])
		}
	}
}

func TestTmuxJumpNoMatchingPaneReturnsToastNamingPath(t *testing.T) {
	msg := tmuxJump(true, func(args ...string) (string, error) {
		return "%1\tsess\t0\t/other/path\n", nil
	}, "/tmp/x")
	if !strings.Contains(msg, "/tmp/x") {
		t.Errorf("tmuxJump = %q, want it to name the unmatched path", msg)
	}
}

func TestTmuxJumpListPanesExecErrorSurfacesAsToastNotPanic(t *testing.T) {
	msg := tmuxJump(true, func(args ...string) (string, error) {
		return "", errors.New(`exec: "tmux": executable file not found in $PATH`)
	}, "/tmp/x")
	if !strings.Contains(msg, "not found") {
		t.Errorf("tmuxJump = %q, want the exec error surfaced verbatim", msg)
	}
}

// TestTmuxJumpSuccessRunsSwitchSelectWindowSelectPaneWithFixedArgv pins
// §2.7 step 3's exact three-exec sequence and argv shape.
func TestTmuxJumpSuccessRunsSwitchSelectWindowSelectPaneWithFixedArgv(t *testing.T) {
	var calls [][]string
	run := func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "list-panes" {
			return "%3\tmysess\t2\t/repo/wt-feature\n", nil
		}
		return "", nil
	}
	msg := tmuxJump(true, run, "/repo/wt-feature")
	if msg != "" {
		t.Fatalf("tmuxJump = %q, want empty (success)", msg)
	}
	want := [][]string{
		{"list-panes", "-a", "-F", tmuxListPanesFormat},
		{"switch-client", "-t", "mysess"},
		{"select-window", "-t", "mysess:2"},
		{"select-pane", "-t", "%3"},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if len(calls[i]) != len(want[i]) {
			t.Fatalf("call %d = %v, want %v", i, calls[i], want[i])
		}
		for j := range want[i] {
			if calls[i][j] != want[i][j] {
				t.Errorf("call %d arg %d = %q, want %q", i, j, calls[i][j], want[i][j])
			}
		}
	}
}

func TestTmuxJumpSwitchClientErrorStopsBeforeSelectWindow(t *testing.T) {
	calls := 0
	run := func(args ...string) (string, error) {
		calls++
		switch args[0] {
		case "list-panes":
			return "%3\tmysess\t2\t/repo/wt\n", nil
		case "switch-client":
			return "", errors.New("no client")
		}
		t.Fatalf("unexpected call after switch-client failed: %v", args)
		return "", nil
	}
	msg := tmuxJump(true, run, "/repo/wt")
	if !strings.Contains(msg, "no client") {
		t.Errorf("tmuxJump = %q, want the switch-client error surfaced", msg)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want exactly 2 (list-panes, switch-client)", calls)
	}
}

// ---- tmuxJumpCmd / $TMUX-absent behavior ----

// TestTmuxJumpCmdWithoutTMUXEnvReturnsToastDoneMsg pins the "$TMUX-absent
// behavior test" the brief asks for at the tea.Cmd boundary (not just the
// pure tmuxJump function): with TMUX unset, the resulting tmuxDoneMsg
// carries a non-nil Err naming the absence, and never touches the exec seam.
func TestTmuxJumpCmdWithoutTMUXEnvReturnsToastDoneMsg(t *testing.T) {
	t.Setenv("TMUX", "")
	cmd := tmuxJumpCmd("/some/worktree")
	msg, ok := cmd().(tmuxDoneMsg)
	if !ok {
		t.Fatalf("tmuxJumpCmd()() = %#v, want a tmuxDoneMsg", msg)
	}
	if msg.Err == nil || !strings.Contains(msg.Err.Error(), "not inside tmux") {
		t.Errorf("tmuxDoneMsg.Err = %v, want it to say not inside tmux", msg.Err)
	}
}
