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
	DefaultBase    string        // "" => use each repo's own default branch
	ActivityWindow time.Duration // how recently a change counts as "active"
}

// meta is the engine's per-worktree cache: enough to skip re-diffing unchanged
// worktrees and to answer Diff()/review queries without touching git again.
type meta struct {
	id         string
	repo       string
	repoPath   string // owning repo root (main worktree), needed for the merge path
	name       string
	path       string
	branch     string
	base       string
	token      string
	diff       model.Diff
	lastChange time.Time
}

// Engine is the orchestrator.
type Engine struct {
	cfg Config
	be  gitbackend.Backend
	reg *registry.Registry
	st  store.Store
	gr  *guardrail.Engine

	refreshMu sync.Mutex // serialises full scans
	mu        sync.RWMutex
	cache     map[string]*meta
}

// New constructs an Engine.
func New(cfg Config, be gitbackend.Backend, reg *registry.Registry, st store.Store, gr *guardrail.Engine) *Engine {
	if cfg.ActivityWindow <= 0 {
		cfg.ActivityWindow = 30 * time.Second
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = 4
	}
	return &Engine{cfg: cfg, be: be, reg: reg, st: st, gr: gr, cache: map[string]*meta{}}
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

// ApproveResult summarises a completed approve.
type ApproveResult struct {
	WorktreeID string `json:"worktreeId"`
	Merged     string `json:"merged"`  // feature branch
	Into       string `json:"into"`    // base branch
	Removed    string `json:"removed"` // removed worktree path
}

// Approve is the single write path. It is deliberately gated: every file in the
// worktree's diff must be marked reviewed, and the worktree must be clean (all work
// committed — you cannot merge uncommitted or untracked changes). It then merges the
// worktree's branch into the base branch (in whichever worktree has base checked
// out) and removes the worktree. If the merge fails it is aborted, leaving base
// untouched, and the error is returned — nothing is removed.
func (e *Engine) Approve(id string) (ApproveResult, error) {
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

	// Re-check under refreshMu: a concurrent Refresh may have removed this
	// worktree (it holds refreshMu for its whole scan) while we waited for the
	// lock. If so, that scan already published the removal — nothing to do,
	// and resurrecting it here would race the removal.
	e.mu.RLock()
	prev, ok := e.cache[id]
	e.mu.RUnlock()
	if !ok {
		return nil
	}

	branch, err := e.be.CurrentBranch(prev.path)
	if err != nil {
		branch = prev.branch // worktree may be mid-operation; keep the last known branch
	}
	repo := model.Repo{Name: prev.repo, Path: prev.repoPath}
	ref := gitbackend.WorktreeRef{Path: prev.path, Branch: branch}
	e.refreshWorktree(repo, ref, prev.base, id)
	return nil
}

// Refresh performs a full scan: discover repos, expand worktrees, and for any whose
// git state changed, recompute the diff, guardrails and stats. It is safe to call
// concurrently — scans are serialised.
func (e *Engine) Refresh(ctx context.Context) error {
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()

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
		base := e.cfg.DefaultBase
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
		diff:       model.Diff{WorktreeID: id, Base: base, Hash: hash, Files: files},
		lastChange: lastChange,
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

	e.reg.Upsert(e.buildWorktree(m, ref))
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

// buildWorktree assembles the public model from cached meta + live review state.
func (e *Engine) buildWorktree(m *meta, ref gitbackend.WorktreeRef) model.Worktree {
	var stats model.Stats
	for _, f := range m.diff.Files {
		stats.Add += f.Stats.Add
		stats.Del += f.Stats.Del
	}
	stats.Files = len(m.diff.Files)

	hits := e.gr.Eval(m.diff)
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

// fileHash computes a per-file diff identity: sha1 over the file's path, status,
// and every hunk line (kind + content). Unlike the whole-diff hash it is
// unaffected by any other file changing, and — because it doesn't include hunk
// start line numbers — by unrelated shifts elsewhere in the same file's hunks;
// it only moves when this file's own content actually changes.
func fileHash(f model.DiffFile) string {
	var b strings.Builder
	b.WriteString(f.Path)
	b.WriteByte('\n')
	b.WriteString(string(f.Status))
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
