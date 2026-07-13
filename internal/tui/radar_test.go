package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// ---- diffLRU ----

func TestDiffLRUMissThenHitAfterPut(t *testing.T) {
	d := newDiffLRU(4)
	if _, ok := d.get("a"); ok {
		t.Fatal("get on an empty LRU should miss")
	}
	d.put("a", model.Diff{WorktreeID: "a"})
	got, ok := d.get("a")
	if !ok || got.WorktreeID != "a" {
		t.Errorf("get after put = %+v, ok=%v, want WorktreeID a", got, ok)
	}
}

// TestDiffLRUEvictsLeastRecentlyUsedPastCap pins "last 4 fetched model.Diff"
// (P3-design.md §2.5).
func TestDiffLRUEvictsLeastRecentlyUsedPastCap(t *testing.T) {
	d := newDiffLRU(2)
	d.put("a", model.Diff{WorktreeID: "a"})
	d.put("b", model.Diff{WorktreeID: "b"})
	d.put("c", model.Diff{WorktreeID: "c"}) // over cap: evicts "a", the LRU
	if _, ok := d.get("a"); ok {
		t.Error("a should have been evicted")
	}
	if _, ok := d.get("b"); !ok {
		t.Error("b should have survived")
	}
	if _, ok := d.get("c"); !ok {
		t.Error("c should have survived")
	}
}

func TestDiffLRUPutOnExistingIDUpdatesAndPromotes(t *testing.T) {
	d := newDiffLRU(2)
	d.put("a", model.Diff{WorktreeID: "a", Hash: "h1"})
	d.put("b", model.Diff{WorktreeID: "b"})
	d.put("a", model.Diff{WorktreeID: "a", Hash: "h2"}) // re-put: promotes a, b is now LRU
	d.put("c", model.Diff{WorktreeID: "c"})             // evicts b, not a
	if _, ok := d.get("b"); ok {
		t.Error("b should have been evicted as the least-recently-used entry")
	}
	got, ok := d.get("a")
	if !ok || got.Hash != "h2" {
		t.Errorf("get(a) = %+v ok=%v, want the updated Hash h2", got, ok)
	}
}

// TestNewRadarViewDiffLRUCapMatchesDesignBudget pins the *production* diffLRU
// cap against P3-design.md §6's budget-table row ("plus max 4 cached
// model.Diffs") — the eviction tests above deliberately construct a
// newDiffLRU with a small synthetic cap directly, so nothing else asserts
// the real constant newRadarView() wires up hasn't drifted.
func TestNewRadarViewDiffLRUCapMatchesDesignBudget(t *testing.T) {
	r := newRadarView()
	if r.diffs.cap != 4 {
		t.Errorf("newRadarView().diffs.cap = %d, want 4 (P3-design.md §6)", r.diffs.cap)
	}
}

// ---- radarView.ensureDiff ----

func TestEnsureDiffDispatchesFetchOnCacheMiss(t *testing.T) {
	r := newRadarView()
	api := &fakeAPI{diff: model.Diff{WorktreeID: "w1", Hash: "h1"}}
	cmd := r.ensureDiff(context.Background(), api, "w1")
	if cmd == nil {
		t.Fatal("expected a fetch command on a cache miss")
	}
	msg := cmd()
	dm, ok := msg.(diffMsg)
	if !ok || dm.ID != "w1" {
		t.Fatalf("cmd() = %#v, want a diffMsg for w1", msg)
	}
}

func TestEnsureDiffSwapsInstantlyFromCacheWithoutACommand(t *testing.T) {
	r := newRadarView()
	r.diffs.put("w1", model.Diff{WorktreeID: "w1", Hash: "cached"})
	cmd := r.ensureDiff(context.Background(), &fakeAPI{}, "w1")
	if cmd != nil {
		t.Error("a cache hit should swap the pane in with no fetch command")
	}
	if r.pane.diff.WorktreeID != "w1" {
		t.Errorf("pane.diff.WorktreeID = %q, want w1 to be showing already", r.pane.diff.WorktreeID)
	}
}

func TestEnsureDiffNoOpWhenAlreadyCurrentAndNoError(t *testing.T) {
	r := newRadarView()
	api := &fakeAPI{diff: model.Diff{WorktreeID: "w1"}}
	first := r.ensureDiff(context.Background(), api, "w1")
	if first == nil {
		t.Fatal("first call should fetch")
	}
	second := r.ensureDiff(context.Background(), api, "w1")
	if second != nil {
		t.Error("re-checking the same, already-current, non-errored id should no-op")
	}
}

// TestEnsureDiffRetriesOnReselectionAfterAPriorError pins the pane's whole
// "retry hint": navigating back to a worktree whose last fetch failed must
// retry, not silently stay stuck on the error.
func TestEnsureDiffRetriesOnReselectionAfterAPriorError(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	r.diffErr = "boom"
	api := &fakeAPI{diff: model.Diff{WorktreeID: "w1"}}
	cmd := r.ensureDiff(context.Background(), api, "w1")
	if cmd == nil {
		t.Fatal("expected a retry fetch when currentID matches but the last attempt errored")
	}
}

func TestEnsureDiffEmptyIDIsNoOp(t *testing.T) {
	r := newRadarView()
	if cmd := r.ensureDiff(context.Background(), &fakeAPI{}, ""); cmd != nil {
		t.Error("empty id should never dispatch a fetch")
	}
}

// ---- applyDiffMsg / applyDiffErrMsg: rapid-navigation correctness ----

// TestApplyDiffMsgForStaleSelectionIsCachedButNotDisplayed pins §2.4's
// order-independence: a diff for a worktree the user has since navigated
// away from must not clobber what's on screen, but should still be cached
// for next time.
func TestApplyDiffMsgForStaleSelectionIsCachedButNotDisplayed(t *testing.T) {
	r := newRadarView()
	r.currentID = "w2" // user has already moved on to w2
	r.applyDiffMsg(diffMsg{ID: "w1", Diff: model.Diff{WorktreeID: "w1"}})

	if r.pane.diff.WorktreeID == "w1" {
		t.Error("a stale diffMsg must not overwrite the pane")
	}
	if _, ok := r.diffs.get("w1"); !ok {
		t.Error("the stale result should still be cached for a future revisit")
	}
}

func TestApplyDiffMsgForCurrentSelectionUpdatesPaneAndClearsError(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	r.diffErr = "previous failure"
	r.applyDiffMsg(diffMsg{ID: "w1", Diff: model.Diff{WorktreeID: "w1"}})
	if r.pane.diff.WorktreeID != "w1" {
		t.Error("pane should now show w1's diff")
	}
	if r.diffErr != "" {
		t.Errorf("diffErr = %q, want cleared on a successful load", r.diffErr)
	}
}

// TestApplyDiffMsgPreservesScrollPositionAcrossASameWorktreeRefetch was
// DEFECT D4 (fixed). diffview.setDiff used to unconditionally reset offset
// to 0 on every load, and applyDiffMsg calls setDiff for every diffMsg,
// including a diff.ready-triggered refetch of a worktree already open and
// scrolled into — not just the first load. A live agent worktree firing
// diff.ready on every unrelated commit would silently snap the user back to
// line 1 mid-review. This was inconsistent with the same pane's own
// fold/expand overrides, which *are* preserved across a refetch (keyed by
// file hash, per flatten.go/diffview.go) — "where you are" in a file
// survived, "where you're scrolled to" didn't. Fix: setDiff only resets to
// the top when the WorktreeID actually changes; a same-worktree refetch now
// preserves offset (re-clamped against the new line count).
func TestApplyDiffMsgPreservesScrollPositionAcrossASameWorktreeRefetch(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	r.applyDiffMsg(diffMsg{ID: "w1", Diff: manyLineDiff(500)})
	r.pane.setHeight(20)
	r.pane.scroll(300) // scrolled deep into the file, mid-review
	before := r.pane.offset
	if before == 0 {
		t.Fatal("precondition: should have scrolled away from offset 0")
	}

	// An unrelated diff.ready refetch lands (e.g. a trivial edit elsewhere in
	// the worktree) while the user is mid-review; the file/line count here is
	// deliberately unchanged.
	r.applyDiffMsg(diffMsg{ID: "w1", Diff: manyLineDiff(500)})

	if r.pane.offset != before {
		t.Errorf("scroll offset = %d after a same-worktree refetch, want it preserved at %d", r.pane.offset, before)
	}
}

func TestApplyDiffErrMsgOnlySetsErrorForCurrentSelection(t *testing.T) {
	r := newRadarView()
	r.currentID = "w2"
	r.applyDiffErrMsg(diffErrMsg{ID: "w1", Err: errors.New("boom")})
	if r.diffErr != "" {
		t.Errorf("diffErr = %q, want empty (error was for a stale selection)", r.diffErr)
	}

	r.currentID = "w1"
	r.applyDiffErrMsg(diffErrMsg{ID: "w1", Err: errors.New("boom")})
	if r.diffErr != "boom" {
		t.Errorf("diffErr = %q, want boom", r.diffErr)
	}
}

// ---- applyDiffReady: hash-gated refetch ----

func TestApplyDiffReadyIgnoredForNonCurrentWorktree(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	cmd := r.applyDiffReady(context.Background(), &fakeAPI{}, model.Event{Type: model.EventDiffReady, ID: "other", Hash: "h2"})
	if cmd != nil {
		t.Error("diff.ready for a different worktree must not trigger a refetch")
	}
}

func TestApplyDiffReadyIgnoredWhenHashUnchanged(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	r.diffs.put("w1", model.Diff{WorktreeID: "w1", Hash: "h1"})
	cmd := r.applyDiffReady(context.Background(), &fakeAPI{}, model.Event{Type: model.EventDiffReady, ID: "w1", Hash: "h1"})
	if cmd != nil {
		t.Error("diff.ready with an unchanged hash must be debounced away")
	}
}

func TestApplyDiffReadyRefetchesOnHashChange(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	r.diffs.put("w1", model.Diff{WorktreeID: "w1", Hash: "h1"})
	api := &fakeAPI{diff: model.Diff{WorktreeID: "w1", Hash: "h2"}}
	cmd := r.applyDiffReady(context.Background(), api, model.Event{Type: model.EventDiffReady, ID: "w1", Hash: "h2"})
	if cmd == nil {
		t.Fatal("diff.ready with a changed hash must refetch")
	}
	msg := cmd()
	if dm, ok := msg.(diffMsg); !ok || dm.Diff.Hash != "h2" {
		t.Errorf("cmd() = %#v, want a diffMsg carrying the new hash", msg)
	}
}

// ---- guardrail banner / per-file severity tag ----

// TestFileSeverityGradesWorstPerFile pins ux-expert P1-1a: each named file
// gets its OWN worst severity ("danger" beats "warn"), not a single "any hit
// = danger" bool — a warn-only rule (deps-manifest-changed, edits-ci,
// lockfile-churn, large-deletion, binary-added, secrets-entropy) must no
// longer paint its file red.
func TestFileSeverityGradesWorstPerFile(t *testing.T) {
	got := fileSeverity([]model.GuardrailHit{
		{File: "a.go", Severity: "warn"},
		{File: "b.go", Severity: "danger"},
		{File: "b.go", Severity: "warn"}, // b.go also warn-tagged elsewhere: danger still wins
		{File: "", Severity: "danger"},   // no file named: not a per-file tag
	})
	if got["a.go"] != "warn" {
		t.Errorf("fileSeverity[a.go] = %q, want warn", got["a.go"])
	}
	if got["b.go"] != "danger" {
		t.Errorf("fileSeverity[b.go] = %q, want danger (worst of warn+danger)", got["b.go"])
	}
	if len(got) != 2 {
		t.Errorf("fileSeverity = %v, want exactly 2 entries", got)
	}
}

func TestRenderGuardrailBannerEmptyWhenNoHits(t *testing.T) {
	if got := renderGuardrailBanner(80, nil); got != "" {
		t.Errorf("banner with no hits = %q, want empty", got)
	}
}

func TestRenderGuardrailBannerShowsMessageVerbatimAndMoreCount(t *testing.T) {
	hits := []model.GuardrailHit{
		{Rule: "r1", Severity: "warn", Message: "touches migrations/"},
		{Rule: "r2", Severity: "danger", Message: "deletes too much"},
	}
	got := stripANSI(renderGuardrailBanner(80, hits))
	if !strings.Contains(got, "deletes too much") {
		t.Errorf("banner = %q, want the danger-severity hit's message verbatim", got)
	}
	if !strings.Contains(got, "+1 more") {
		t.Errorf("banner = %q, want a \"+1 more\" suffix for the second hit", got)
	}
}

// TestRenderGuardrailBannerAppendsFileLineForContentConditionHits pins
// P5-design.md §1.6's WP3 TUI touch: a content-condition hit (secrets-pattern/
// secrets-entropy) carries a Line, and the banner appends "· file:line" for
// it so a secrets hit is jumpable-by-eye.
func TestRenderGuardrailBannerAppendsFileLineForContentConditionHits(t *testing.T) {
	hits := []model.GuardrailHit{
		{Rule: "secrets-pattern", Severity: "danger", Message: "secrets-shaped string", File: "internal/auth/token.go", Line: 42},
	}
	// A wide width, not 80: the banner box word-wraps at its own width, and
	// the point here is the suffix's presence/content, not line-wrapping.
	got := stripANSI(renderGuardrailBanner(200, hits))
	if !strings.Contains(got, "· internal/auth/token.go:42") {
		t.Errorf("banner = %q, want a \"· file:line\" suffix for a hit carrying Line", got)
	}
}

// TestRenderGuardrailBannerOmitsFileLineWhenHitHasNoLine pins the other half:
// a per-file hit with no content condition (e.g. touches-migrations) never
// set Line, and today's banner has never named a file at all — that must not
// regress into always showing one now that the field exists.
func TestRenderGuardrailBannerOmitsFileLineWhenHitHasNoLine(t *testing.T) {
	hits := []model.GuardrailHit{
		{Rule: "touches-migrations", Severity: "danger", Message: "touches database migrations", File: "migrations/x.sql"},
	}
	got := stripANSI(renderGuardrailBanner(200, hits))
	if strings.Contains(got, "·") {
		t.Errorf("banner = %q, want no \"· file:line\" suffix when the featured hit has no Line", got)
	}
}

// TestRenderGuardrailBannerSanitizesControlBytesInMessage pins the belt-and-
// braces control-byte defense: a hit's Message can be hand-authored in a
// repo's own .wtcockpit.toml pack (semi-trusted, P5-design.md §1.3) and so
// never passes through diffparse's own sanitizing pass the way diff content
// does — a raw ESC byte must render as caret notation, never reach the
// terminal as a live escape sequence.
func TestRenderGuardrailBannerSanitizesControlBytesInMessage(t *testing.T) {
	hits := []model.GuardrailHit{
		{Rule: "hostile-pack-rule", Severity: "danger", Message: "trigger\x1b[31mred\x1b[0m"},
	}
	got := renderGuardrailBanner(80, hits)
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("banner contains a raw ESC byte, want it caret-sanitized: %q", got)
	}
	if !strings.Contains(stripANSI(got), "^[") {
		t.Errorf("banner = %q, want the sanitized ESC byte rendered as caret notation \"^[\"", stripANSI(got))
	}
}

// TestRenderGuardrailBannerGradesByWorstSeverity pins ux-expert P1-1b: the
// diff-pane banner must grade red for a danger-severity featured hit and
// amber for a warn-only one — matching its own sidebar badge (severityBadge,
// sidebar_test.go's TestSeverityBadgeWorstSeverityWins) and the web room
// banner instead of always rendering amber regardless of severity. Compares
// under TrueColor (the package's TestMain forces Ascii, which strips all
// styling) so the two grades are actually visibly distinct: each case is
// checked for containing its own severity's styled "⚠ guardrail: " prefix
// (not the other's) rather than a whole-string ref, since the mixed case's
// extra "+N more" suffix would otherwise make it differ from a single-hit
// danger fixture even with identical grading.
func TestRenderGuardrailBannerGradesByWorstSeverity(t *testing.T) {
	saved := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(saved)
	lipgloss.SetColorProfile(termenv.TrueColor)

	dangerPrefix := styles.Del.Bold(true).Render("⚠ guardrail: ")
	warnPrefix := styles.Warn.Bold(true).Render("⚠ guardrail: ")
	if dangerPrefix == warnPrefix {
		t.Fatal("precondition: styles.Del and styles.Warn must render distinctly under TrueColor")
	}

	cases := []struct {
		name       string
		hits       []model.GuardrailHit
		wantDanger bool
	}{
		{"warn only", []model.GuardrailHit{{Severity: "warn", Message: "m"}}, false},
		{"danger only", []model.GuardrailHit{{Severity: "danger", Message: "m"}}, true},
		{"mixed: worst (danger) wins", []model.GuardrailHit{{Severity: "warn", Message: "m1"}, {Severity: "danger", Message: "m2"}}, true},
	}
	for _, c := range cases {
		got := renderGuardrailBanner(80, c.hits)
		want, notWant := warnPrefix, dangerPrefix
		if c.wantDanger {
			want, notWant = dangerPrefix, warnPrefix
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s: renderGuardrailBanner = %q, missing its expected severity-graded prefix", c.name, got)
		}
		if strings.Contains(got, notWant) {
			t.Errorf("%s: renderGuardrailBanner = %q, must not use the other severity's prefix", c.name, got)
		}
	}
}

// ---- diff pane header ----

// TestRenderDiffHeaderSanitizesControlBytesInRepoAndName is P7 security
// LOW-1's pin: w.Repo/w.Name are filepath.Base(dir) — filesystem-derived text
// that can carry raw control bytes on Unix — and renderDiffHeader used to
// print both raw.
func TestRenderDiffHeaderSanitizesControlBytesInRepoAndName(t *testing.T) {
	w := model.Worktree{Repo: "evil\x1b]0;pwned\x07-repo", Name: "evil\x1bname", Base: "main"}
	got := renderDiffHeader(80, w, false, false)
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Fatalf("raw ESC/BEL bytes leaked into the diff header: %q", got)
	}
	if !strings.Contains(stripANSI(got), "evil^[]0;pwned^G-repo") {
		t.Errorf("header = %q, want the caret-sanitized repo name", stripANSI(got))
	}
	if !strings.Contains(stripANSI(got), "evil^[name") {
		t.Errorf("header = %q, want the caret-sanitized worktree name", stripANSI(got))
	}
}

func TestRenderDiffHeaderContainsRepoNameAndBase(t *testing.T) {
	w := model.Worktree{Repo: "api-server", Name: "auth-refactor", Base: "main", Stats: model.Stats{Files: 3, Add: 10, Del: 2}}
	got := stripANSI(renderDiffHeader(80, w, false, false))
	for _, want := range []string{"api-server", "auth-refactor", "main", "3 files"} {
		if !strings.Contains(got, want) {
			t.Errorf("header = %q, want it to contain %q", got, want)
		}
	}
}

// TestRenderDiffHeaderAccentsTitleWhenFocused pins the ux-expert P2-3 focus
// cue: Radar's two focus states (sidebar vs. diff pane) otherwise render
// identically, so the header title is the one visible signal distinguishing
// them. Compares under TrueColor (the package's TestMain forces Ascii, which
// renders no escape codes at all — see fake_test.go) so a styling
// difference is actually observable in the byte output.
func TestRenderDiffHeaderAccentsTitleWhenFocused(t *testing.T) {
	saved := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(saved)
	lipgloss.SetColorProfile(termenv.TrueColor)

	w := model.Worktree{Repo: "api-server", Name: "auth-refactor", Base: "main"}
	unfocused := renderDiffHeader(80, w, false, false)
	focused := renderDiffHeader(80, w, true, false)
	if unfocused == focused {
		t.Error("a focused header should render styled differently from an unfocused one")
	}
	if stripANSI(unfocused) != stripANSI(focused) {
		t.Errorf("focus must only change styling, not content: unfocused=%q focused=%q", stripANSI(unfocused), stripANSI(focused))
	}
}

// TestRenderDiffHeaderReviewingShowsBranchVsBase pins the ux-expert P3-cheap
// review-header seam: Review's call (reviewing=true) reads "reviewing
// <branch> vs <base>", matching the mock's `#rv-base` treatment, instead of
// Radar's plain "base <base>".
func TestRenderDiffHeaderReviewingShowsBranchVsBase(t *testing.T) {
	w := model.Worktree{Repo: "api", Name: "feature", Branch: "feature-x", Base: "main"}

	reviewing := stripANSI(renderDiffHeader(80, w, false, true))
	if !strings.Contains(reviewing, "reviewing feature-x vs main") {
		t.Errorf("reviewing header = %q, want it to contain %q", reviewing, "reviewing feature-x vs main")
	}

	radar := stripANSI(renderDiffHeader(80, w, false, false))
	if strings.Contains(radar, "reviewing") {
		t.Errorf("radar header = %q, must not say \"reviewing\"", radar)
	}
	if !strings.Contains(radar, "base main") {
		t.Errorf("radar header = %q, want the plain \"base main\" segment preserved", radar)
	}
}

// TestRenderDiffHeaderNeverEndsInADanglingSeparator is the ux-expert
// regression's property test (p3-frames-2/frame_07): a width-starved header
// — Review's diff pane is width-railWidth, far narrower than Radar's own
// full main pane — used to blindly clipWidth the whole title+base+stats+chip
// line, which could cut the trailing agent chip off mid-glyph and leave the
// "· " that introduces it dangling with nothing after it ("1 files · +1 -1
// ·" butted straight against the rail). Sweeps every width from 0 up to
// comfortably past each fixture's own full unclipped length, both reviewing
// states, and two worktrees (a short one, and one whose branch/base/agent
// are all long enough to stress every segment) — the invariant must hold
// regardless of exactly where a given width happens to cut the line.
func TestRenderDiffHeaderNeverEndsInADanglingSeparator(t *testing.T) {
	worktrees := []model.Worktree{
		{Repo: "api", Name: "feature", Branch: "feature", Base: "main", Agent: model.AgentClaude, Stats: model.Stats{Files: 3, Add: 10, Del: 2}},
		{
			Repo: "api-server", Name: "a-rather-long-worktree-name-indeed",
			Branch: "a-rather-long-branch-name-indeed", Base: "release/a-rather-long-base-name",
			Agent: model.AgentAider, Stats: model.Stats{Files: 123456, Add: 999999, Del: 999999},
		},
	}
	for _, w := range worktrees {
		for _, reviewing := range []bool{false, true} {
			full := stripANSI(renderDiffHeader(10000, w, false, reviewing))
			maxWidth := lipgloss.Width(full) + 2
			for width := 0; width <= maxWidth; width++ {
				got := stripANSI(renderDiffHeader(width, w, false, reviewing))
				if trimmed := strings.TrimRight(got, " "); strings.HasSuffix(trimmed, "·") {
					t.Fatalf("renderDiffHeader(width=%d, reviewing=%v, %+v) = %q, ends in a dangling separator", width, reviewing, w, got)
				}
			}
		}
	}
}

// ---- radarView.view: loading / error / real content ----

func TestRadarViewShowsLoadingPlaceholderBeforeDiffArrives(t *testing.T) {
	r := newRadarView()
	w := model.Worktree{ID: "w1", Repo: "api", Name: "feature", Base: "main"}
	out := stripANSI(r.view(80, 24, w, false, false))
	if !strings.Contains(out, "loading diff") {
		t.Errorf("view = %q, want a loading placeholder before any diff has loaded", out)
	}
}

func TestRadarViewShowsErrorInsteadOfPane(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	r.diffErr = "unknown worktree id"
	w := model.Worktree{ID: "w1", Repo: "api", Name: "feature", Base: "main"}
	out := stripANSI(r.view(80, 24, w, false, false))
	if !strings.Contains(out, "unknown worktree id") {
		t.Errorf("view = %q, want the daemon's error message", out)
	}
}

func TestRadarViewRendersLoadedDiffWithGuardrailBanner(t *testing.T) {
	r := newRadarView()
	d := model.Diff{WorktreeID: "w1", Files: []model.DiffFile{
		{Path: "migrations/x.sql", Status: model.FileModified, Hash: "h1", Stats: model.Stats{Add: 1, Del: 90},
			Hunks: []model.Hunk{{Header: "@@ -1,90 +1,1 @@", Lines: []model.Line{line(model.LineDel, 1, 0, "DROP TABLE x")}}}},
	}}
	r.currentID = "w1"
	r.pane.setDiff(d)
	w := model.Worktree{
		ID: "w1", Repo: "infra", Name: "migrate", Base: "main",
		Guardrails: []model.GuardrailHit{{Rule: "big-delete", Severity: "danger", Message: "deletes too much", File: "migrations/x.sql"}},
	}
	out := stripANSI(r.view(80, 24, w, false, false))
	if !strings.Contains(out, "deletes too much") {
		t.Errorf("view = %q, want the guardrail banner", out)
	}
	if !strings.Contains(out, "migrations/") || !strings.Contains(out, "x.sql") {
		t.Errorf("view = %q, want the file card", out)
	}
	if !strings.Contains(out, "danger") {
		t.Errorf("view = %q, want the file's danger tag", out)
	}
}

// TestRadarViewAtMinimumHeightWithBannerSqueezesPaneWithoutPanicking is the
// "resize to a 1-row height mid-scroll" edge: app.go's minHeight=10 guard
// means shellView() (and so radarView.view) never actually sees a height
// below 10 -- a true 1-row terminal shows the "too small" card instead. The
// realistic worst case that *does* reach this composition is the documented
// minimum (10 rows) with a multi-line guardrail banner squeezing the pane
// itself down to just 1-2 rows, while the user was already scrolled deep
// into a large diff before the resize. Regression guard: no panic, exact
// requested height, and the scroll offset is left in a valid (if no longer
// visibly meaningful) range by setHeight's own clampOffset.
func TestRadarViewAtMinimumHeightWithBannerSqueezesPaneWithoutPanicking(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	r.applyDiffMsg(diffMsg{ID: "w1", Diff: manyLineDiff(500)})
	r.pane.setHeight(40)
	r.pane.scroll(300) // scrolled deep in before the resize

	w := model.Worktree{
		ID: "w1", Repo: "infra", Name: "migrate", Base: "main",
		Guardrails: []model.GuardrailHit{{Rule: "r1", Severity: "danger", Message: "a fairly long guardrail message to eat into the pane's height budget", File: "big.go"}},
	}

	out := r.view(80, minHeight, w, false, false) // minHeight=10: app.go's documented floor
	lines := strings.Count(out, "\n") + 1
	if lines != minHeight {
		t.Errorf("radarView.view height = %d lines, want exactly %d", lines, minHeight)
	}
	maxOffset := len(r.pane.lines) - r.pane.height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if r.pane.offset < 0 || r.pane.offset > maxOffset {
		t.Errorf("pane.offset = %d after the squeeze, want it re-clamped within [0, %d]", r.pane.offset, maxOffset)
	}
}
