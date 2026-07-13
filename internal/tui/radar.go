package tui

import (
	"container/list"
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/navbytes/wt-cockpit/internal/diffparse"
	"github.com/navbytes/wt-cockpit/internal/model"
)

// diffLRUCap is "keep an LRU of the last 4 fetched model.Diff so switching
// between worktrees is instant" (P3-design.md §2.5).
const diffLRUCap = 4

// diffLRU caches recently-fetched model.Diff by worktree id. Only ever
// touched from the Update()/View() goroutine (bubbletea's single event
// loop) — no mutex needed, unlike highlightCache.
type diffLRU struct {
	cap int
	ll  *list.List
	idx map[string]*list.Element
}

type diffLRUEntry struct {
	id   string
	diff model.Diff
}

func newDiffLRU(cap int) diffLRU {
	return diffLRU{cap: cap, ll: list.New(), idx: map[string]*list.Element{}}
}

func (d *diffLRU) get(id string) (model.Diff, bool) {
	el, ok := d.idx[id]
	if !ok {
		return model.Diff{}, false
	}
	d.ll.MoveToFront(el)
	return el.Value.(*diffLRUEntry).diff, true
}

func (d *diffLRU) put(id string, diff model.Diff) {
	if el, ok := d.idx[id]; ok {
		el.Value = &diffLRUEntry{id: id, diff: diff}
		d.ll.MoveToFront(el)
		return
	}
	el := d.ll.PushFront(&diffLRUEntry{id: id, diff: diff})
	d.idx[id] = el
	if d.ll.Len() > d.cap {
		if back := d.ll.Back(); back != nil {
			delete(d.idx, back.Value.(*diffLRUEntry).id)
			d.ll.Remove(back)
		}
	}
}

// radarView composes the Radar screen's main pane (P3-design.md §1.1/§2.3):
// header, guardrail banner, then the virtualized unified diff. It owns one
// diffview plus the small diff/highlight caches that make revisiting a
// worktree instant.
type radarView struct {
	pane    diffview
	hl      *highlightCache
	diffs   diffLRU
	pending map[string]bool // file hashes with an in-flight highlight request

	currentID string // worktree id the pane is loading/showing
	diffErr   string // non-empty => show an inline error instead of the pane
}

func newRadarView() radarView {
	return radarView{hl: newHighlightCache(), pending: map[string]bool{}, diffs: newDiffLRU(diffLRUCap)}
}

// ensureDiff makes sure the pane is showing (or fetching) id's diff: a cache
// hit swaps instantly with no round trip; otherwise it dispatches a fetch.
// Re-selecting the same id after a prior failure retries rather than no-op,
// which is this pane's whole "retry hint" (P3-design.md §1.4): navigate away
// and back, or `R` (which independently forces a full refresh), to retry.
func (r *radarView) ensureDiff(ctx context.Context, api apiClient, id string) tea.Cmd {
	if id == "" {
		return nil
	}
	if cached, ok := r.diffs.get(id); ok {
		r.currentID = id
		r.pane.setDiff(cached)
		r.diffErr = ""
		return nil
	}
	if id == r.currentID && r.diffErr == "" {
		return nil
	}
	r.currentID = id
	r.diffErr = ""
	return fetchDiffCmd(ctx, api, id)
}

// applyDiffMsg caches every diff that lands, but only swaps it into the
// visible pane if the user hasn't since navigated to a different worktree —
// order-independent w.r.t. how fast rapid navigation fires off requests.
func (r *radarView) applyDiffMsg(msg diffMsg) {
	r.diffs.put(msg.ID, msg.Diff)
	if msg.ID == r.currentID {
		r.pane.setDiff(msg.Diff)
		r.diffErr = ""
	}
}

func (r *radarView) applyDiffErrMsg(msg diffErrMsg) {
	if msg.ID == r.currentID {
		r.diffErr = msg.Err.Error()
	}
}

func (r *radarView) applyHighlighted(msg highlightedMsg) {
	r.hl.put(msg.FileHash, msg.Lines)
	delete(r.pending, msg.FileHash)
}

// applyDiffReady is the SSE diff.ready{id,hash} delta (P3-design.md §2.4):
// refetch only when it names the currently-open worktree AND the hash
// actually differs from what's cached — "debounced by the hash check".
func (r *radarView) applyDiffReady(ctx context.Context, api apiClient, e model.Event) tea.Cmd {
	if e.ID != r.currentID {
		return nil
	}
	if cached, ok := r.diffs.get(e.ID); ok && cached.Hash == e.Hash {
		return nil
	}
	return fetchDiffCmd(ctx, api, e.ID)
}

func (r *radarView) ensureHighlightsCmd() tea.Cmd {
	return ensureHighlightCmds(&r.pane, r.hl, r.pending)
}

// applyReviewOK reconciles the pane's optimistic toggle with the server's
// confirmation (P3-design.md §1.5's "server truth reconciliation") — a
// no-op in the common case (it already matches what was optimistically
// set), self-correcting if a rapid double-toggle raced it.
func (r *radarView) applyReviewOK(msg reviewOKMsg) {
	if msg.ID != r.currentID {
		return
	}
	r.pane.setReviewed(msg.File, msg.Reviewed)
}

// applyReviewErr reverts the optimistic toggle for any failure by inverting
// msg.Reviewed — the fixed value that toggle attempted to set, carried on
// the message itself — rather than whatever Reviewed[file] currently holds
// (DEFECT D3: an interleaved diff.ready refetch can have already replaced
// that with fresher, unrelated server truth by the time a stale error
// arrives, so inverting "current" can clobber it). Only for the 409
// conflict case, also refetches the diff, since the file's *content*
// changed too, not just its reviewed flag (P3-design.md §1.4's "diff
// refreshed").
func (r *radarView) applyReviewErr(ctx context.Context, api apiClient, msg reviewErrMsg) tea.Cmd {
	if msg.ID == r.currentID {
		r.pane.setReviewed(msg.File, !msg.Reviewed)
	}
	if msg.Conflict {
		return fetchDiffCmd(ctx, api, msg.ID)
	}
	return nil
}

// view renders the Radar main pane for the selected worktree w: header,
// guardrail banner (when tripped), then the virtualized diff (or a loading/
// error placeholder while the pane isn't yet showing w's diff). focused
// accents the header title as a focus cue when the diff pane (rather than
// the sidebar) holds the keyboard (ux-expert P2-3); reviewing swaps the
// header's base segment to Review's "reviewing <branch> vs <base>" phrasing
// (ux-expert P3-cheap) — Radar's own call passes false/false, Review's
// passes false/true (its own screen has no separate diff-focus state to
// track, so there's nothing for that pane to further distinguish).
func (r *radarView) view(width, height int, w model.Worktree, focused, reviewing bool) string {
	header := renderDiffHeader(width, w, focused, reviewing)
	banner := renderGuardrailBanner(width, w.Guardrails)

	paneH := height - lipgloss.Height(header)
	if banner != "" {
		paneH -= lipgloss.Height(banner)
	}
	if paneH < 1 {
		paneH = 1
	}

	// Keep the pane's known height current even in the loading/error branches
	// below (which don't call render()), so the instant real data lands,
	// ensureHighlightsCmd already knows the true viewport to check against.
	r.pane.setHeight(paneH)

	var body string
	switch {
	case r.diffErr != "":
		body = r.errorView(width, paneH)
	case r.pane.diff.WorktreeID != w.ID:
		body = lipgloss.NewStyle().Width(width).Height(paneH).Render(styles.Dim.Render("loading diff…"))
	default:
		body = r.pane.render(width, paneH, r.hl, fileSeverity(w.Guardrails))
	}

	parts := []string{header}
	if banner != "" {
		parts = append(parts, banner)
	}
	parts = append(parts, body)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (r *radarView) errorView(width, height int) string {
	lines := []string{
		styles.Del.Render("failed to load diff: " + r.diffErr),
		styles.Dim.Render("↑/↓ to reselect, or R to refresh — either retries"),
	}
	return lipgloss.NewStyle().Width(width).Height(height).Render(strings.Join(lines, "\n"))
}

// renderDiffHeader is the mock's "pane-h": repo/name, base branch, and the
// files/±/agent summary (P3-design.md §1.1). focused accents the title as a
// lightweight focus cue (ux-expert P2-3): Radar's two focus states (sidebar
// vs. diff pane) otherwise look identical, so the one owning the keyboard
// gets the accent color the rest of the app already uses for "this is what
// you're driving" (selection, links). reviewing swaps the base segment to
// Review's own "reviewing <branch> vs <base>" phrasing (ux-expert P3-cheap,
// mock's `#rv-base` treatment) instead of Radar's plain "base <base>".
func renderDiffHeader(width int, w model.Worktree, focused, reviewing bool) string {
	titleStyle := styles.Txt.Bold(true)
	if focused {
		titleStyle = styles.Accent.Bold(true)
	}
	// w.Repo/w.Name are filepath.Base(dir) — filesystem-derived, can carry raw
	// control bytes on Unix (P7 security LOW-1) — sanitized at this render
	// sink, same treatment the guardrail banner's Message already gets below.
	title := titleStyle.Render(fmt.Sprintf("%s / %s", diffparse.SanitizeControl(w.Repo), diffparse.SanitizeControl(w.Name)))

	var base string
	if reviewing {
		base = styles.Dim.Render("reviewing ") + styles.Accent.Render(w.Branch) +
			styles.Dim.Render(" vs ") + styles.Accent.Render(w.Base)
	} else {
		base = styles.Dim.Render("base ") + styles.Accent.Render(w.Base)
	}

	stats := fmt.Sprintf("%d files · %s %s",
		w.Stats.Files,
		styles.Add.Render(fmt.Sprintf("+%d", w.Stats.Add)),
		styles.Del.Render(fmt.Sprintf("-%d", w.Stats.Del)),
	)
	line := title + "   " + base + "   " + stats

	// The agent chip is the header's lowest-priority, trailing segment.
	// Review's diff pane (width - railWidth) is far narrower than Radar's
	// full main pane, so a bare clipWidth on the whole line was much more
	// likely to cut it off mid-glyph there, leaving the "· " that introduces
	// it dangling with nothing after it (ux-expert regression,
	// p3-frames-2/frame_07: "1 files · +1 -1 ·" butted straight against the
	// rail). Appended only whole, dropped entirely otherwise — never partial.
	chip := " · " + agentChipStyle(w.Agent).Render(string(w.Agent))
	if lipgloss.Width(line+chip) <= width {
		line += chip
	}

	return trimDanglingSeparator(clipWidth(line, width))
}

// trimDanglingSeparator strips a trailing "·" (the header's own segment
// joiner, with or without the space that follows it in the untruncated
// string) left dangling when clipWidth's width-aware cut lands right after
// one — a backstop alongside the chip's own whole-or-nothing append above,
// for whichever width happens to cut the line at that exact boundary
// instead. The header must never end in a separator with nothing after it.
func trimDanglingSeparator(s string) string {
	s = strings.TrimRight(s, " ")
	s = strings.TrimSuffix(s, "·")
	return strings.TrimRight(s, " ")
}

// renderGuardrailBanner is the mock's alert box: the featured hit's rule
// message verbatim, plus — when the featured hit carries a Line (a
// content-condition hit: secrets-pattern/secrets-entropy, P5-design.md
// §1.1/§1.6) — a "· file:line" suffix so it's jumpable-by-eye, then a "+N
// more" suffix when several rules tripped. Tinted red (styles.Del/DelBg) when
// the featured hit (worst severity present, picked below) is "danger", else
// amber (styles.Warn/WarnBg) — ux-expert P1-1b: this used to always render
// amber regardless of severity, contradicting its own sidebar badge
// (severityBadge) and the web room banner, both of which already grade.
//
// featured.Message runs through diffparse.SanitizeControl before rendering.
// Diff content/paths are already sanitized upstream by diffparse.Parse (the
// same defensive-reuse this package's own flatten.go already applies to hunk
// text), but a hit's Message can instead be hand-authored directly in a
// repo's own .wtcockpit.toml pack — semi-trusted input (P5-design.md §1.3)
// that never passes through that pipeline — so without this, a raw control
// byte smuggled into one would reach the terminal unescaped.
func renderGuardrailBanner(width int, hits []model.GuardrailHit) string {
	if len(hits) == 0 {
		return ""
	}
	featured := hits[0]
	for _, h := range hits {
		if h.Severity == "danger" {
			featured = h
			break
		}
	}
	fg, bg := styles.Warn, styles.WarnBg
	if featured.Severity == "danger" {
		fg, bg = styles.Del, styles.DelBg
	}
	text := fg.Bold(true).Render("⚠ guardrail: ") + styles.Txt.Render(diffparse.SanitizeControl(featured.Message))
	if featured.Line > 0 {
		text += styles.Dim.Render(fmt.Sprintf(" · %s:%d", featured.File, featured.Line))
	}
	if len(hits) > 1 {
		text += styles.Dim.Render(fmt.Sprintf(" (+%d more)", len(hits)-1))
	}
	return bg.Width(width).Padding(0, 1).Render(text)
}

// fileSeverity computes each named file's own WORST guardrail severity
// ("danger" beats "warn") — ux-expert P1-1a: the file header's tag now
// carries this instead of a fixed "any hit = danger" bool, so a warn-only
// rule (deps-manifest-changed, edits-ci, lockfile-churn, large-deletion,
// binary-added, secrets-entropy) no longer paints its file red. Grading
// matches severityBadge/renderGuardrailBanner's own "danger wins" rule.
func fileSeverity(hits []model.GuardrailHit) map[string]string {
	out := make(map[string]string, len(hits))
	for _, h := range hits {
		if h.File == "" {
			continue
		}
		sev := "warn"
		if h.Severity == "danger" {
			sev = "danger"
		}
		if sev == "danger" || out[h.File] == "" {
			out[h.File] = sev
		}
	}
	return out
}

func fetchDiffCmd(ctx context.Context, api apiClient, id string) tea.Cmd {
	return func() tea.Msg {
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		d, err := api.Diff(reqCtx, id)
		if err != nil {
			return diffErrMsg{ID: id, Err: err}
		}
		return diffMsg{ID: id, Diff: d}
	}
}
