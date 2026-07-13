package main

// wt menubar: the SwiftBar/xbar plugin-format emitter (P5-design.md §1.7).
// requireWtBin/unixSocketServer/shortSocketDir come from integration_test.go,
// same package.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// ---- pure formatting: writeMenubar/menubarLabel/dangerCount ----

func TestDangerCountCountsOnlyDangerSeverityHits(t *testing.T) {
	hits := []model.GuardrailHit{
		{Rule: "secrets-entropy", Severity: "warn"},
		{Rule: "secrets-pattern", Severity: "danger"},
		{Rule: "ci-workflow-delete", Severity: "danger"},
	}
	if got := dangerCount(hits); got != 2 {
		t.Errorf("dangerCount = %d, want 2", got)
	}
	if got := dangerCount(nil); got != 0 {
		t.Errorf("dangerCount(nil) = %d, want 0", got)
	}
}

// TestMenubarLabelTableDriven covers the row-label priority rule (danger
// beats unreviewed-count) and the "nothing to show" empty case, across the
// richer v0.5 hit shapes.
func TestMenubarLabelTableDriven(t *testing.T) {
	cases := []struct {
		name string
		wt   model.Worktree
		want string
	}{
		{"fully reviewed, no hits", model.Worktree{Stats: model.Stats{Files: 3}, Reviewed: 3}, ""},
		{"unreviewed files, no hits", model.Worktree{Stats: model.Stats{Files: 5}, Reviewed: 2}, "3 files unreviewed"},
		{
			"one danger hit, fully reviewed",
			model.Worktree{Stats: model.Stats{Files: 4}, Reviewed: 4, Guardrails: []model.GuardrailHit{{Rule: "secrets-pattern", Severity: "danger"}}},
			"1 danger",
		},
		{
			"danger hit AND unreviewed files: danger wins",
			model.Worktree{
				Stats: model.Stats{Files: 4}, Reviewed: 1,
				Guardrails: []model.GuardrailHit{{Rule: "ci-workflow-delete", Severity: "danger"}, {Rule: "huge-churn", Severity: "warn"}},
			},
			"1 danger",
		},
		{
			"warn-only hits, fully reviewed: no row (warn alone isn't attention-worthy)",
			model.Worktree{Stats: model.Stats{Files: 2}, Reviewed: 2, Guardrails: []model.GuardrailHit{{Rule: "lockfile-churn", Severity: "warn"}}},
			"",
		},
	}
	for _, c := range cases {
		if got := menubarLabel(c.wt); got != c.want {
			t.Errorf("%s: menubarLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

func fixtureMenubarWorktrees() []model.Worktree {
	return []model.Worktree{
		{ID: "wt-a", Repo: "api-server", Name: "auth-refactor", Stats: model.Stats{Files: 4}, Reviewed: 4,
			Guardrails: []model.GuardrailHit{{Rule: "secrets-pattern", Severity: "danger"}}},
		{ID: "wt-b", Repo: "web", Name: "checkout-flow", Stats: model.Stats{Files: 5}, Reviewed: 2},
		{ID: "wt-c", Repo: "web", Name: "all-clean", Stats: model.Stats{Files: 3}, Reviewed: 3},
	}
}

// TestWriteMenubarFormatsTitleRowsAndOpenCockpit pins §1.7's frozen shape end
// to end: title line, one row per worktree needing attention (sorted
// repo/name, each with an href), the fully-clean worktree omitted from rows
// but still folded into the title's aggregate, and a trailing "Open cockpit"
// link.
func TestWriteMenubarFormatsTitleRowsAndOpenCockpit(t *testing.T) {
	var buf bytes.Buffer
	writeMenubar(&buf, fixtureMenubarWorktrees(), "127.0.0.1:7788")
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	if len(lines) != 6 {
		t.Fatalf("got %d lines, want 6 (title, ---, 2 rows, ---, open):\n%s", len(lines), out)
	}
	// title: 1 danger worktree (wt-a only, even though it's just 1 hit),
	// reviewed/total = (4+2+3)/(4+5+3) = 9/12.
	if lines[0] != "⚠1 ✓9/12" {
		t.Errorf("title = %q, want \"⚠1 ✓9/12\"", lines[0])
	}
	if lines[1] != "---" {
		t.Errorf("line 2 = %q, want \"---\"", lines[1])
	}
	if lines[2] != "api-server/auth-refactor — 1 danger | href=http://127.0.0.1:7788/wt/wt-a" {
		t.Errorf("row 1 = %q", lines[2])
	}
	if lines[3] != "web/checkout-flow — 3 files unreviewed | href=http://127.0.0.1:7788/wt/wt-b" {
		t.Errorf("row 2 = %q", lines[3])
	}
	if strings.Contains(out, "all-clean") {
		t.Errorf("the fully-reviewed, hit-free worktree must not get its own row, got:\n%s", out)
	}
	if lines[4] != "---" {
		t.Errorf("line 5 = %q, want the second \"---\"", lines[4])
	}
	if lines[5] != "Open cockpit | href=http://127.0.0.1:7788/" {
		t.Errorf("last line = %q, want the Open cockpit link", lines[5])
	}
}

// TestWriteMenubarOmitsHrefWhenWebAddrEmpty pins the "-web off / P4 not
// merged" degradation (§1.7): every row and the trailing link render as
// plain, unclickable labels — no "href=" anywhere.
func TestWriteMenubarOmitsHrefWhenWebAddrEmpty(t *testing.T) {
	var buf bytes.Buffer
	writeMenubar(&buf, fixtureMenubarWorktrees(), "")
	out := buf.String()
	if strings.Contains(out, "href=") {
		t.Errorf("expected no href anywhere when webAddr is empty, got:\n%s", out)
	}
	if !strings.Contains(out, "api-server/auth-refactor — 1 danger\n") {
		t.Errorf("expected the plain (unlinked) row, got:\n%s", out)
	}
	if !strings.Contains(out, "Open cockpit\n") {
		t.Errorf("expected the plain (unlinked) Open cockpit row, got:\n%s", out)
	}
}

// TestWriteMenubarEmptyFleetStillEmitsValidPluginText: zero worktrees is a
// normal state (nothing to review yet), not an error — the title/separators/
// Open-cockpit row must still all render.
func TestWriteMenubarEmptyFleetStillEmitsValidPluginText(t *testing.T) {
	var buf bytes.Buffer
	writeMenubar(&buf, nil, "127.0.0.1:7788")
	out := buf.String()
	if !strings.HasPrefix(out, "⚠0 ✓0/0\n---\n") {
		t.Errorf("expected the empty-fleet title+separator, got:\n%s", out)
	}
	if !strings.Contains(out, "Open cockpit | href=http://127.0.0.1:7788/") {
		t.Errorf("expected the Open cockpit link even with zero worktrees, got:\n%s", out)
	}
}

// ---- full binary round-trip against a fake daemon ----

func menubarFakeDaemonMux(t *testing.T, webAddr string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": model.ProtocolVersion, "version": "x", "goVersion": "go1.24"})
	})
	mux.HandleFunc("/api/worktrees", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(fixtureMenubarWorktrees())
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"webAddr": webAddr})
	})
	return mux
}

// wtMenubarTitleRE parses the title line's format for a structural (not just
// substring) check: "parse the emitted SwiftBar text" per the phase's own
// test list.
var wtMenubarTitleRE = regexp.MustCompile(`^⚠\d+ ✓\d+/\d+$`)

func TestWtMenubarBinaryRoundTripAgainstFakeDaemon(t *testing.T) {
	bin := requireWtBin(t)
	sockPath := filepath.Join(shortSocketDir(t), "wtd.sock")
	ts := unixSocketServer(t, sockPath, menubarFakeDaemonMux(t, "127.0.0.1:7788"))
	defer ts.Close()

	cmd := exec.Command(bin, "menubar")
	cmd.Env = append(os.Environ(), "WTD_SOCKET="+sockPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("wt menubar failed: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}

	out := stdout.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 2 || !wtMenubarTitleRE.MatchString(lines[0]) {
		t.Fatalf("title line = %q, want it to match %s\nfull output:\n%s", lines[0], wtMenubarTitleRE, out)
	}
	if got := strings.Count(out, "---\n"); got != 2 {
		t.Errorf("expected exactly 2 \"---\" separators, got %d in:\n%s", got, out)
	}
	if !strings.Contains(out, "api-server/auth-refactor — 1 danger | href=http://127.0.0.1:7788/wt/wt-a") {
		t.Errorf("missing the danger worktree row, got:\n%s", out)
	}
	if !strings.Contains(out, "Open cockpit | href=http://127.0.0.1:7788/") {
		t.Errorf("missing the Open cockpit row, got:\n%s", out)
	}
}

// TestWtMenubarOmitsHrefWhenWebAddrEmptyBinary is the binary-level twin of
// TestWriteMenubarOmitsHrefWhenWebAddrEmpty: a live daemon with -web off (or
// predating it) still produces valid, useful plugin text, just unlinked.
func TestWtMenubarOmitsHrefWhenWebAddrEmptyBinary(t *testing.T) {
	bin := requireWtBin(t)
	sockPath := filepath.Join(shortSocketDir(t), "wtd.sock")
	ts := unixSocketServer(t, sockPath, menubarFakeDaemonMux(t, ""))
	defer ts.Close()

	cmd := exec.Command(bin, "menubar")
	cmd.Env = append(os.Environ(), "WTD_SOCKET="+sockPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("wt menubar failed: %v\nstderr=%s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), "href=") {
		t.Errorf("expected no href with an empty webAddr, got:\n%s", stdout.String())
	}
}

// TestWtMenubarDaemonDownPrintsDegradedTitleAndExitsZero is the phase's other
// explicit test-list item: unlike every other wt command, menubar must never
// exit nonzero on a down daemon — a plugin host treats that as a broken
// plugin — and instead renders the "wt ◦" degraded state.
func TestWtMenubarDaemonDownPrintsDegradedTitleAndExitsZero(t *testing.T) {
	bin := requireWtBin(t)
	sockPath := filepath.Join(shortSocketDir(t), "no-daemon.sock") // nothing listening

	cmd := exec.Command(bin, "menubar")
	cmd.Env = append(os.Environ(), "WTD_SOCKET="+sockPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("wt menubar against a down daemon must exit 0 (xbar-friendly), got err=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	out := stdout.String()
	if !strings.HasPrefix(out, "wt ◦\n---\n") {
		t.Errorf("stdout = %q, want the degraded \"wt ◦\" title + separator", out)
	}
}
