// Package engine is the heart of the daemon. It ties discovery, the git backend,
// the guardrail engine, the review store and the registry together, and exposes a
// small query/command surface that every frontend consumes. It is UI-agnostic: no
// terminal or HTTP concern leaks in here.
package engine

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/navbytes/wt-cockpit/internal/diffparse"
	"github.com/navbytes/wt-cockpit/internal/discovery"
	"github.com/navbytes/wt-cockpit/internal/gitbackend"
	"github.com/navbytes/wt-cockpit/internal/guardrail"
	"github.com/navbytes/wt-cockpit/internal/model"
	"github.com/navbytes/wt-cockpit/internal/registry"
	"github.com/navbytes/wt-cockpit/internal/store"
)

// ErrFileNotFound is returned by SetReviewed when the worktree is unknown or the
// file is not part of its current diff.
var ErrFileNotFound = errors.New("file not found in current diff")

// ErrFileChanged is returned by SetReviewed when the caller's expectedHash no
// longer matches the file's current diff hash: the file changed since it was
// viewed, so the review request is stale.
var ErrFileChanged = errors.New("file changed since it was reviewed")

// Config holds engine settings.
type Config struct {
	Roots          []string
	MaxDepth       int
	DefaultBase    string            // "" => use each repo's own default branch
	BaseFor        map[string]string // repo path -> base override, takes precedence over DefaultBase
	ActivityWindow time.Duration     // how recently a change counts as "active"
}

// meta is the engine's per-worktree cache: enough to skip re-diffing unchanged
// worktrees and to answer Diff()/review queries without touching git again.
// hits is the worktree's guardrail hit set as of its last refresh — kept so
// refreshWorktree can compute which hits are NEW (see publishNewGuardrailHits).
type meta struct {
	id         string
	repo       string
	repoPath   string // owning repo root (main worktree), needed for the merge path AND for pack resolution
	name       string
	path       string
	branch     string
	base       string
	token      string
	diff       model.Diff
	lastChange time.Time
	hits       []model.GuardrailHit
}

// Engine is the orchestrator.
type Engine struct {
	cfg Config
	be  gitbackend.Backend
	reg *registry.Registry
	st  store.Store
	gr  *guardrail.Resolver

	refreshMu sync.Mutex // serialises full scans
	mu        sync.RWMutex
	cache     map[string]*meta

	// firstScanDone gates guardrail.tripped emission (P5-design.md §1.4): no
	// hit-appearance event is published until the engine's first full Refresh
	// completes, so restarting the daemon over a pile of worktrees with
	// standing hits never replays them as "new". Set once, at the end of the
	// first Refresh call, for the engine's whole lifetime.
	firstScanDone atomic.Bool

	// lastRefreshDur/lastRefreshOneDur record the wall-clock duration (as
	// nanoseconds, time.Duration's own unit) of the most recently COMPLETED
	// full Refresh / targeted RefreshOne (P6-design.md §6.3 layer 3 — the
	// v0.2 roadmap's "scan timings" IOU): additive observability only, no
	// behavior change. Atomics so a concurrent /api/status read never races
	// a refresh in flight — it just sees the previous completed value until
	// the new one finishes and stores. Zero means "hasn't completed one yet".
	lastRefreshDur    atomic.Int64
	lastRefreshOneDur atomic.Int64
}

// New constructs an Engine.
func New(cfg Config, be gitbackend.Backend, reg *registry.Registry, st store.Store, gr *guardrail.Resolver) *Engine {
	if cfg.ActivityWindow <= 0 {
		cfg.ActivityWindow = 30 * time.Second
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = 4
	}
	return &Engine{cfg: cfg, be: be, reg: reg, st: st, gr: gr, cache: map[string]*meta{}}
}

// Rules returns the effective, provenance-tagged rule set for worktree id's
// owning repo (P5-design.md §1.3) — GET /api/rules and `wt rules`'s payload.
// ok=false for an unknown worktree, mirroring Diff/WorktreePath's contract.
func (e *Engine) Rules(id string) (guardrail.Effective, bool) {
	e.mu.RLock()
	m, ok := e.cache[id]
	e.mu.RUnlock()
	if !ok {
		return guardrail.Effective{}, false
	}
	return e.gr.Effective(id, m.repoPath), true
}

// RulePackStats reports how many currently-known repos are running a
// validly-loaded pack ("loaded") vs a malformed one that fell back to global
// rules ("errors") — statusPayload's additive `rulePacks` field.
func (e *Engine) RulePackStats() (loaded, errs int) {
	return e.gr.Stats()
}

// Registry exposes the underlying registry (for Subscribe/List in the daemon).
func (e *Engine) Registry() *registry.Registry { return e.reg }

// List returns the current worktrees, most-recently-changed first.
func (e *Engine) List() []model.Worktree { return e.reg.List() }

// Diff returns the cached structured diff for a worktree.
func (e *Engine) Diff(id string) (model.Diff, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	m, ok := e.cache[id]
	if !ok {
		return model.Diff{}, false
	}
	return m.diff, true
}

// WorktreePath returns the filesystem path for a worktree id.
func (e *Engine) WorktreePath(id string) (string, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	m, ok := e.cache[id]
	if !ok {
		return "", false
	}
	return m.path, true
}

// SetReviewed records a per-file review toggle and republishes the worktree so the
// reviewed count updates live for every client. file must be part of the
// worktree's current diff, or ErrFileNotFound is returned. If expectedHash is
// non-empty it must match the file's current diff hash, or ErrFileChanged is
// returned — the file changed since the caller last looked at it. Marking
// reviewed=true records the file's current hash; reviewed=false unreviews it.
func (e *Engine) SetReviewed(id, file string, reviewed bool, expectedHash string) error {
	e.mu.RLock()
	m, ok := e.cache[id]
	e.mu.RUnlock()
	if !ok {
		return ErrFileNotFound
	}
	cur := findDiffFile(m.diff.Files, file)
	if cur == nil {
		return ErrFileNotFound
	}
	if expectedHash != "" && expectedHash != cur.Hash {
		return ErrFileChanged
	}

	var err error
	if reviewed {
		err = e.st.SetReviewed(id, file, cur.Hash)
	} else {
		err = e.st.Unreview(id, file)
	}
	if err != nil {
		return err
	}

	if w := e.reg.Get(id); w != nil {
		rf, _ := e.st.ReviewedFiles(id)
		w.Reviewed = reviewedCount(m.diff.Files, rf)
		e.reg.Upsert(*w)
		e.reg.Publish(model.Event{Type: model.EventReviewChanged, ID: id, At: time.Now()})
	}
	return nil
}

// LastRefreshDurations reports the wall-clock duration of the most recently
// completed full Refresh and targeted RefreshOne (P6-design.md §6.3 layer 3):
// `wt status`/`/api/status`'s perf counters, the v0.2 roadmap's "scan
// timings" IOU. Zero for either means it hasn't completed one yet (e.g. a
// daemon that has never seen a targeted refresh reports one=0 forever).
func (e *Engine) LastRefreshDurations() (full, one time.Duration) {
	return time.Duration(e.lastRefreshDur.Load()), time.Duration(e.lastRefreshOneDur.Load())
}

// ReviewedMap returns, for worktree id's current diff, whether each file is
// currently marked reviewed: true when the file's stored review hash
// matches its present diff hash (the same per-file rule reviewedCount's
// aggregate uses) — a stale hash, i.e. the file changed since it was
// reviewed, reads as false, same as a file that was never reviewed at all.
// Read-only; added so cmd/wtd's GET /api/diff can serve a per-file reviewed
// map (P3-design.md's sanctioned WP3 addition: WP2 found the diff pane's
// per-file ✓ had nothing to key on, since Worktree.Reviewed is only an
// aggregate count). ok=false for an unknown worktree, mirroring Diff's own
// contract.
func (e *Engine) ReviewedMap(id string) (map[string]bool, bool) {
	e.mu.RLock()
	m, ok := e.cache[id]
	e.mu.RUnlock()
	if !ok {
		return nil, false
	}
	reviewed, _ := e.st.ReviewedFiles(id) // jsonStore's ReviewedFiles never errors
	out := make(map[string]bool, len(m.diff.Files))
	for _, f := range m.diff.Files {
		out[f.Path] = reviewed[f.Path] == f.Hash
	}
	return out, true
}

// findDiffFile looks up a file by path within a diff's files.
func findDiffFile(files []model.DiffFile, path string) *model.DiffFile {
	for i := range files {
		if files[i].Path == path {
			return &files[i]
		}
	}
	return nil
}

// reviewedCount counts files whose stored review hash matches their current
// diff hash — a stale hash (the file changed since it was reviewed) doesn't count.
func reviewedCount(files []model.DiffFile, reviewed map[string]string) int {
	n := 0
	for _, f := range files {
		if reviewed[f.Path] == f.Hash {
			n++
		}
	}
	return n
}

// ApproveResult summarises a completed approve. Moved to internal/model (the
// wire-shape package every frontend, including the thin client, can import
// without dragging the engine along) — kept here as an alias so existing
// engine.ApproveResult references keep compiling unchanged.
type ApproveResult = model.ApproveResult

// ApproveMergeConflictError is Approve's error for a real merge conflict
// (P7-ux.md P1-2): a bare git exit-status/conflict dump is not an actionable
// message for the human at the "money moment" of a failed approve. Error()
// is deliberately just the one clear sentence — CLI fatal() and the web
// banner both print whatever Approve returns verbatim, so this is what they
// show. Raw carries git's own conflict detail for cmd/wtd's AUDIT log only
// (via errors.As); it must never be included in Error()'s own text.
type ApproveMergeConflictError struct {
	Branch, Base string
	Raw          error
}

func (e *ApproveMergeConflictError) Error() string {
	return fmt.Sprintf("cannot merge %s into %s: conflicts with the base branch — update your branch and re-review (base unchanged)", e.Branch, e.Base)
}

func (e *ApproveMergeConflictError) Unwrap() error { return e.Raw }

// Approve is the single write path. It is deliberately gated: every file in the
// worktree's diff must be marked reviewed, and the worktree must be clean (all work
// committed — you cannot merge uncommitted or untracked changes). It then merges the
// worktree's branch into the base branch (in whichever worktree has base checked
// out) and removes the worktree. If the merge fails it is aborted, leaving base
// untouched, and the error is returned — nothing is removed.
//
// Approve holds refreshMu for its entire duration (FIX M1): it never trusts a
// possibly-stale cache entry — a commit-after-review landing in the window
// before the next poll/fsnotify-debounced refresh must not slip past the
// review gate — so it re-diffs the worktree fresh first, via the same locked
// helper RefreshOne uses, and evaluates every gate against that fresh diff.
// Holding the lock through the merge/cache mutations at the end also closes
// the second half of M1: a concurrent Refresh/RefreshOne can no longer race
// in and resurrect the worktree this call just merged and removed.
func (e *Engine) Approve(id string) (ApproveResult, error) {
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()

	e.refreshOneLocked(id)

	e.mu.RLock()
	m, ok := e.cache[id]
	e.mu.RUnlock()
	if !ok {
		return ApproveResult{}, fmt.Errorf("unknown worktree %q", id)
	}
	if len(m.diff.Files) == 0 {
		return ApproveResult{}, fmt.Errorf("no changes to approve")
	}

	// Gate 1: fully reviewed — every file's stored hash must match its current
	// hash (a stale hash means the file changed since it was reviewed).
	reviewed, err := e.st.ReviewedFiles(id)
	if err != nil {
		return ApproveResult{}, err
	}
	var unreviewed int
	for _, f := range m.diff.Files {
		if reviewed[f.Path] != f.Hash {
			unreviewed++
		}
	}
	if unreviewed > 0 {
		return ApproveResult{}, fmt.Errorf("cannot approve: %d of %d files not yet reviewed", unreviewed, len(m.diff.Files))
	}

	// Gate 2: clean worktree (everything committed, so the branch is mergeable).
	dirty, err := e.be.IsDirty(m.path)
	if err != nil {
		return ApproveResult{}, err
	}
	if dirty {
		return ApproveResult{}, fmt.Errorf("cannot approve: worktree has uncommitted changes — commit them first")
	}

	// Locate the worktree that has the base branch checked out; that is where the
	// merge must happen (git updates a branch's working tree in place).
	refs, err := e.be.ListWorktrees(m.repoPath)
	if err != nil {
		return ApproveResult{}, err
	}
	basePath := ""
	for _, r := range refs {
		if r.Branch == m.base {
			basePath = r.Path
		}
	}
	if basePath == "" {
		return ApproveResult{}, fmt.Errorf("base branch %q is not checked out in any worktree; cannot merge safely", m.base)
	}
	if basePath == m.path {
		return ApproveResult{}, fmt.Errorf("worktree is on the base branch; nothing to merge")
	}

	// Merge (aborts internally on conflict, leaving base clean).
	if err := e.be.Merge(basePath, m.branch); err != nil {
		if errors.Is(err, gitbackend.ErrMergeConflict) {
			return ApproveResult{}, &ApproveMergeConflictError{Branch: m.branch, Base: m.base, Raw: err}
		}
		return ApproveResult{}, fmt.Errorf("merge failed and was aborted (base unchanged): %w", err)
	}

	// Success: remove the worktree and forget its state.
	_ = e.be.RemoveWorktree(m.repoPath, m.path)
	_ = e.st.ClearWorktree(id)
	e.mu.Lock()
	delete(e.cache, id)
	e.mu.Unlock()
	e.reg.Remove(id)

	return ApproveResult{WorktreeID: id, Merged: m.branch, Into: m.base, Removed: m.path}, nil
}

// RefreshOne re-diffs a single worktree identified by its filesystem path — the
// targeted counterpart to Refresh, so a change hint from the watcher doesn't
// force a full rescan of every repo under every root. worktreePath is looked up
// the same way Refresh derives ids (worktreeID), so it must be the same path
// the engine already knows the worktree by. An unknown path (not yet cached —
// e.g. a brand new worktree the watcher hasn't reconciled into a full Refresh
// yet) falls back to a full Refresh.
//
// ponytail: refresh stays serialized (refreshMu, shared with Refresh); a
// GOMAXPROCS re-diff pool is only worth it once many hot worktrees measurably
// lag under one-at-a-time refreshes.
func (e *Engine) RefreshOne(ctx context.Context, worktreePath string) error {
	id := worktreeID(worktreePath)
	e.mu.RLock()
	_, ok := e.cache[id]
	e.mu.RUnlock()
	if !ok {
		return e.Refresh(ctx)
	}

	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	start := time.Now()
	e.refreshOneLocked(id)
	e.lastRefreshOneDur.Store(int64(time.Since(start))) // only this, the actually-targeted path — the delegate-to-Refresh branch above already updates lastRefreshDur via Refresh itself
	return nil
}

// refreshOneLocked re-diffs the cached worktree identified by id in place.
// Callers must already hold refreshMu — it exists so both RefreshOne (the
// watcher's targeted-refresh path) and Approve (which must never gate on a
// possibly-stale cache entry, per FIX M1) can re-diff a single worktree
// without racing a concurrent Refresh/RefreshOne for the same id. Reports
// whether the worktree was (still) cached; there is nothing to refresh if a
// concurrent Refresh removed it — it holds refreshMu for its whole scan, so
// that removal is already published, and resurrecting the entry here would
// race it.
func (e *Engine) refreshOneLocked(id string) bool {
	e.mu.RLock()
	prev, ok := e.cache[id]
	e.mu.RUnlock()
	if !ok {
		return false
	}

	branch, err := e.be.CurrentBranch(prev.path)
	if err != nil {
		branch = prev.branch // worktree may be mid-operation; keep the last known branch
	}
	repo := model.Repo{Name: prev.repo, Path: prev.repoPath}
	ref := gitbackend.WorktreeRef{Path: prev.path, Branch: branch}
	e.refreshWorktree(repo, ref, prev.base, id)
	return true
}

// Refresh performs a full scan: discover repos, expand worktrees, and for any whose
// git state changed, recompute the diff, guardrails and stats. It is safe to call
// concurrently — scans are serialised.
//
// The first call to Refresh in the engine's lifetime to actually COMPLETE
// marks firstScanDone at the end — this is the cold-start gate that stops a
// daemon restart from replaying every standing guardrail hit as "new" (see
// publishNewGuardrailHits). A Refresh that fails outright (a discovery
// failure, or ctx canceled mid-scan) leaves the gate closed rather than
// opening it on a scan that never populated a baseline: opening it anyway
// would make the NEXT (successful) scan republish every standing hit as new,
// exactly the restart storm this gate exists to prevent. A worktree
// discovered by a LATER Refresh (or by RefreshOne, e.g. one an agent just
// created) still publishes normally on its own first eval, since the gate is
// already open by then.
func (e *Engine) Refresh(ctx context.Context) error {
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	start := time.Now()

	repos, err := discovery.DiscoverRepos(e.cfg.Roots, e.cfg.MaxDepth)
	if err != nil {
		return err
	}

	seen := map[string]bool{}
	baseByRepo := map[string]string{}

	for _, repo := range repos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		base := e.cfg.BaseFor[repo.Path]
		if base == "" {
			base = e.cfg.DefaultBase
		}
		if base == "" {
			if b, ok := baseByRepo[repo.Path]; ok {
				base = b
			} else {
				base, _ = e.be.DefaultBranch(repo.Path)
				if base == "" {
					base = "main"
				}
				baseByRepo[repo.Path] = base
			}
		}

		refs, err := e.be.ListWorktrees(repo.Path)
		if err != nil {
			continue // a bad repo shouldn't sink the whole scan
		}
		for _, ref := range refs {
			id := worktreeID(ref.Path)
			seen[id] = true
			e.refreshWorktree(repo, ref, base, id)
		}
	}

	// Remove worktrees that disappeared from disk.
	for id := range e.reg.IDs() {
		if !seen[id] {
			e.mu.Lock()
			delete(e.cache, id)
			e.mu.Unlock()
			e.reg.Remove(id)
		}
	}
	e.firstScanDone.Store(true)                      // only on this, the success path — see doc comment above
	e.lastRefreshDur.Store(int64(time.Since(start))) // same success-only rule as firstScanDone above: a fast-failing scan must not overwrite a real duration with a misleadingly small one
	return nil
}

// refreshWorktree recomputes one worktree's diff and updates the registry. Under
// the polling watcher we recompute the diff each scan (cheap for normal repos, and
// correct — a content edit to an already-dirty file must be detected). The whole-
// diff *hash* only drives lastChange/the diff.ready event; review state lives at
// the per-file level (see model.DiffFile.Hash) and survives a whole-diff hash
// change on its own — notably a commit inside the worktree, which doesn't alter
// any file's content and so leaves every per-file hash exactly where it was. A
// future fsnotify watcher can reinstate a skip-if-unchanged fast path keyed on
// .git/index + worktree mtimes.
//
// Guardrails are evaluated once here (rather than inside buildWorktree, as
// before v0.5) because this is the one place that can compare the new hit set
// against the worktree's PREVIOUS one (meta.hits) and publish a
// guardrail.tripped event per hit whose (rule, file) key is genuinely new —
// see publishNewGuardrailHits. repo.Path (the repo's main worktree root, per
// discovery) is what Resolver.For reads a .wtcockpit.toml pack from — never
// ref.Path, the worktree actually being diffed, which is what keeps an agent
// from weakening the guardrails judging its own diff (P5-design.md §1.3).
func (e *Engine) refreshWorktree(repo model.Repo, ref gitbackend.WorktreeRef, base, id string) {
	diffText, derr := e.be.DiffAgainstBase(ref.Path, base)
	if derr != nil {
		diffText = "" // treat as no diff rather than dropping the worktree
	}
	files := diffparse.Parse(diffText)
	for i := range files {
		files[i].Hash = fileHash(files[i])
	}
	hash := hashString(diffText)
	diff := model.Diff{WorktreeID: id, Base: base, Hash: hash, Files: files}
	hits := e.gr.For(repo.Path).Eval(diff)

	e.mu.RLock()
	prev, cached := e.cache[id]
	e.mu.RUnlock()

	now := time.Now()
	lastChange := now
	changed := true
	if cached && prev.diff.Hash == hash && prev.base == base {
		lastChange = prev.lastChange
		changed = false
	}

	m := &meta{
		id:         id,
		repo:       repo.Name,
		repoPath:   repo.Path,
		name:       branchName(ref),
		path:       ref.Path,
		branch:     ref.Branch,
		base:       base,
		token:      hash,
		diff:       diff,
		lastChange: lastChange,
		hits:       hits,
	}
	e.mu.Lock()
	e.cache[id] = m
	e.mu.Unlock()

	// Files that dropped out of the diff (deleted, merged away, or edited back
	// to match base) can never be hash-matched again — drop their stale review
	// records rather than let them linger in the store forever.
	e.pruneReviews(id, files)

	if changed {
		e.reg.Publish(model.Event{Type: model.EventDiffReady, ID: id, Hash: hash, At: now})
	}
	e.publishNewGuardrailHits(id, prev, hits, now)

	e.reg.Upsert(e.buildWorktree(m, ref, hits))
}

// hitKey identifies a guardrail hit for new-vs-standing comparison. Line is
// deliberately excluded (P5-design.md §1.4): an agent editing above an
// existing match must not re-trip the same (rule, file) hit.
type hitKey struct{ rule, file string }

// publishNewGuardrailHits publishes one guardrail.tripped event per hit in
// hits whose (rule, file) key was absent from prev's hit set — the delta the
// bus ships, never full state. Suppressed entirely until the engine's first
// full Refresh completes (firstScanDone): a daemon restart re-evaluating a
// pile of worktrees with standing hits must not replay them all as "new".
func (e *Engine) publishNewGuardrailHits(id string, prev *meta, hits []model.GuardrailHit, now time.Time) {
	if !e.firstScanDone.Load() {
		return
	}
	var prevHits []model.GuardrailHit
	if prev != nil {
		prevHits = prev.hits
	}
	seen := make(map[hitKey]bool, len(prevHits))
	for _, h := range prevHits {
		seen[hitKey{h.Rule, h.File}] = true
	}
	for _, h := range hits {
		if seen[hitKey{h.Rule, h.File}] {
			continue
		}
		hc := h
		e.reg.Publish(model.Event{Type: model.EventGuardrail, ID: id, Hit: &hc, At: now})
	}
}

// pruneReviews removes stored review records whose path is no longer present in
// the current diff.
func (e *Engine) pruneReviews(id string, files []model.DiffFile) {
	reviewed, err := e.st.ReviewedFiles(id)
	if err != nil || len(reviewed) == 0 {
		return
	}
	keep := make(map[string]bool, len(files))
	for _, f := range files {
		keep[f.Path] = true
	}
	for path := range reviewed {
		if !keep[path] {
			_ = e.st.Unreview(id, path)
		}
	}
}

// buildWorktree assembles the public model from cached meta + live review
// state. hits is passed in (computed once by refreshWorktree, alongside the
// new-hit delta publish) rather than re-evaluated here.
func (e *Engine) buildWorktree(m *meta, ref gitbackend.WorktreeRef, hits []model.GuardrailHit) model.Worktree {
	var stats model.Stats
	for _, f := range m.diff.Files {
		stats.Add += f.Stats.Add
		stats.Del += f.Stats.Del
	}
	stats.Files = len(m.diff.Files)

	reviewed, _ := e.st.ReviewedFiles(m.id)
	dirty, _ := e.be.IsDirty(m.path)

	return model.Worktree{
		ID:         m.id,
		Repo:       m.repo,
		Name:       m.name,
		Path:       m.path,
		Branch:     m.branch,
		Base:       m.base,
		Agent:      detectAgent(m.path),
		State:      e.state(stats, dirty, m.lastChange),
		Stats:      stats,
		LastChange: m.lastChange,
		DiffHash:   m.diff.Hash,
		Guardrails: hits,
		Reviewed:   reviewedCount(m.diff.Files, reviewed),
	}
}

func (e *Engine) state(s model.Stats, dirty bool, lastChange time.Time) model.WorktreeState {
	if s.Add == 0 && s.Del == 0 {
		return model.StateIdle
	}
	if time.Since(lastChange) < e.cfg.ActivityWindow {
		return model.StateActive
	}
	if dirty {
		return model.StateDirty
	}
	return model.StateIdle
}

// detectAgent is a best-effort, non-authoritative guess based on marker files an
// agent tends to leave in a worktree. Unknown is a fine default.
func detectAgent(path string) model.AgentKind {
	exists := func(p string) bool { _, err := os.Stat(filepath.Join(path, p)); return err == nil }
	switch {
	case exists(".claude"):
		return model.AgentClaude
	case exists(".aider.conf.yml") || exists(".aider.chat.history.md"):
		return model.AgentAider
	case exists(".codex") || exists("AGENTS.md"):
		return model.AgentCodex
	default:
		return model.AgentUnknown
	}
}

func branchName(ref gitbackend.WorktreeRef) string {
	if ref.Branch != "" {
		return ref.Branch
	}
	return filepath.Base(ref.Path)
}

// worktreeID is a stable id derived from the absolute worktree path.
func worktreeID(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	sum := sha1.Sum([]byte(abs))
	return hex.EncodeToString(sum[:6]) // 12 hex chars, plenty for local uniqueness
}

func hashString(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// fileHash computes a per-file diff identity: sha1 over the file's path,
// status, old/new blob object ids (from the diff's "index a..b" line, when
// git provided one), and every hunk line (kind + content). The blob ids are
// what make this safe for files with zero hunks: a binary diff (or a
// 100%-similarity rename) never renders hunks, so hunk lines alone would
// leave the hash pinned to path+status no matter how much the bytes changed
// — see DEFECT/FIX B1. Unlike the whole-diff hash it is unaffected by any
// other file changing, and — because it doesn't include hunk start line
// numbers — by unrelated shifts elsewhere in the same file's hunks; it only
// moves when this file's own content actually changes.
func fileHash(f model.DiffFile) string {
	var b strings.Builder
	b.WriteString(f.Path)
	b.WriteByte('\n')
	b.WriteString(f.OldPath)
	b.WriteByte('\n')
	b.WriteString(string(f.Status))
	b.WriteByte('\n')
	b.WriteString(f.OldBlob)
	b.WriteByte('\n')
	b.WriteString(f.NewBlob)
	b.WriteByte('\n')
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			switch l.Kind {
			case model.LineAdd:
				b.WriteByte('+')
			case model.LineDel:
				b.WriteByte('-')
			default:
				b.WriteByte(' ')
			}
			b.WriteString(l.Content)
			b.WriteByte('\n')
		}
	}
	return hashString(b.String())
}
