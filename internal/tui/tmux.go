package tui

import (
	"errors"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// tmuxPane is one row of `tmux list-panes -a -F` output (P3-design.md §2.7's
// stable-since-tmux-2.x format).
type tmuxPane struct {
	id      string // #{pane_id}, e.g. "%3"
	session string // #{session_name}
	window  string // #{window_index}
	cwd     string // #{pane_current_path}
}

// tmuxListPanesFormat is the exact -F format string P3-design.md §2.7 names;
// parseTmuxPanes splits each line on tab in this field order.
const tmuxListPanesFormat = "#{pane_id}\t#{session_name}\t#{window_index}\t#{pane_current_path}"

// parseTmuxPanes parses `tmux list-panes -a -F` output (one pane per line,
// tab-separated per tmuxListPanesFormat). A malformed line (wrong field
// count — never expected from a real tmux, but cheap to tolerate) is
// skipped rather than aborting the whole parse.
func parseTmuxPanes(output string) []tmuxPane {
	var panes []tmuxPane
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			continue
		}
		panes = append(panes, tmuxPane{id: f[0], session: f[1], window: f[2], cwd: f[3]})
	}
	return panes
}

// pickTmuxPane implements P3-design.md §2.7 step 2. An exact cwd match on
// the worktree path wins outright; otherwise it picks the pane whose cwd is
// a *descendant* of the worktree path ("agents cd into subdirs" — the
// parenthetical that disambiguates which side of the two paths is the
// prefix), preferring the most deeply nested match when several panes are
// open somewhere under the same worktree; ties (equal depth) keep whichever
// was scanned first.
func pickTmuxPane(panes []tmuxPane, worktreePath string) (tmuxPane, bool) {
	var best tmuxPane
	bestLen := -1
	for _, p := range panes {
		if p.cwd == worktreePath {
			return p, true
		}
		if isUnderDir(worktreePath, p.cwd) && len(p.cwd) > bestLen {
			best, bestLen = p, len(p.cwd)
		}
	}
	return best, bestLen >= 0
}

// isUnderDir reports whether cwd is a (possibly deep) subdirectory of dir,
// comparing whole path segments so "/repo/wt-other" is not a false-positive
// match for dir "/repo/wt".
func isUnderDir(dir, cwd string) bool {
	return dir != "" && strings.HasPrefix(cwd, strings.TrimRight(dir, "/")+"/")
}

// tmuxExec is the swappable exec seam (P3-design.md §2.7): runTmux in
// production, a canned func in tests. Args are always passed straight
// through to exec.Command's argv, never a shell — a worktree path can never
// be interpreted as shell syntax.
type tmuxExec func(args ...string) (string, error)

func runTmux(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).Output()
	return string(out), err
}

// tmuxJump is the pure orchestration behind the `t` key (P3-design.md §2.7),
// parameterised over the exec seam and the $TMUX lookup so it's fully
// unit-testable without a real tmux session. The returned string is a toast
// message; empty means "jumped successfully, nothing to say".
func tmuxJump(insideTmux bool, run tmuxExec, worktreePath string) string {
	if !insideTmux {
		return "not inside tmux"
	}
	out, err := run("list-panes", "-a", "-F", tmuxListPanesFormat)
	if err != nil {
		return err.Error()
	}
	pane, ok := pickTmuxPane(parseTmuxPanes(out), worktreePath)
	if !ok {
		return "no tmux pane is cd'd into " + worktreePath
	}
	if _, err := run("switch-client", "-t", pane.session); err != nil {
		return err.Error()
	}
	if _, err := run("select-window", "-t", pane.session+":"+pane.window); err != nil {
		return err.Error()
	}
	if _, err := run("select-pane", "-t", pane.id); err != nil {
		return err.Error()
	}
	return ""
}

// tmuxJumpCmd is the `t` key's tea.Cmd: exec'd off the Update goroutine, so
// it never blocks the UI (P3-design.md §2.7). A non-nil Err on the resulting
// tmuxDoneMsg is a toast, never a crash.
func tmuxJumpCmd(worktreePath string) tea.Cmd {
	return func() tea.Msg {
		if msg := tmuxJump(os.Getenv("TMUX") != "", runTmux, worktreePath); msg != "" {
			return tmuxDoneMsg{Err: errors.New(msg)}
		}
		return tmuxDoneMsg{}
	}
}
