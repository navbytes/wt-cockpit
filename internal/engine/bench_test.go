// bench_test.go is §6.2's performance suite (P6-design.md §6.2, §6.3, WP3):
// testing.B benchmarks for the engine/daemon paths the design budgets, using
// a stub gitbackend.Backend (canned diff text, zero subprocess spawns) so
// git cost is excluded and only the engine+store's own refresh-path cost is
// measured — plus the matching 10×-budget smoke gates (WT_BENCH_GATE only,
// §6.3 layer 2; a plain `go test ./...` must skip every TestBudget* here).
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/gitbackend"
	"github.com/navbytes/wt-cockpit/internal/guardrail"
	"github.com/navbytes/wt-cockpit/internal/registry"
	"github.com/navbytes/wt-cockpit/internal/store"
)

// stubBackend is a canned-answer gitbackend.Backend (P6-design.md §6.2): every
// method returns a fixed, valid-shaped value with zero I/O and zero
// subprocess spawns — the isolation the design calls for ("store overhead...
// with a stub gitbackend.Backend so git cost is excluded"). Only
// ListWorktrees and DiffAgainstBase are ever exercised by Refresh's own code
// path for these benchmarks; the rest exist solely to satisfy the interface.
type stubBackend struct {
	worktrees []gitbackend.WorktreeRef
	diffText  string
}

func (s *stubBackend) ListWorktrees(repoPath string) ([]gitbackend.WorktreeRef, error) {
	return s.worktrees, nil
}
func (s *stubBackend) CurrentBranch(worktreePath string) (string, error)   { return "feature", nil }
func (s *stubBackend) DefaultBranch(repoPath string) (string, error)       { return "main", nil }
func (s *stubBackend) MergeBase(worktreePath, base string) (string, error) { return "deadbeef", nil }
func (s *stubBackend) IsDirty(worktreePath string) (bool, error)           { return false, nil }
func (s *stubBackend) StateToken(worktreePath string) (string, error)      { return "token", nil }
func (s *stubBackend) Merge(baseWorktreePath, branch string) error         { return nil }
func (s *stubBackend) RemoveWorktree(gitDir, targetPath string) error      { return nil }
func (s *stubBackend) DiffAgainstBase(worktreePath, base string) (string, error) {
	return s.diffText, nil
}

// cannedDiffText builds a valid, parseable unified diff touching nFiles
// distinct files (one small hunk each) — enough for diffparse.Parse and the
// per-file hash/guardrail eval Refresh always runs to do real, but cheap,
// work, so the benchmark isolates git-subprocess cost specifically (the
// design's stated goal) rather than becoming a diffparse microbenchmark.
func cannedDiffText(nFiles int) string {
	var b strings.Builder
	for i := 0; i < nFiles; i++ {
		path := fmt.Sprintf("pkg/file%03d.go", i)
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", path, path)
		fmt.Fprintf(&b, "index 0000000..%07x 100644\n", i+1)
		fmt.Fprintf(&b, "--- a/%s\n", path)
		fmt.Fprintf(&b, "+++ b/%s\n", path)
		fmt.Fprintf(&b, "@@ -1,1 +1,2 @@\n")
		fmt.Fprintf(&b, " package pkg\n")
		fmt.Fprintf(&b, "+func g%d() int { return %d }\n", i, i)
	}
	return b.String()
}

// mustResolverTB is engine_test.go's mustResolver, generalized to
// testing.TB so *testing.B benchmarks can share it too (test helpers aren't
// importable across packages, so per this repo's own convention — see
// sqlite_store_test.go's header comment — this is a small, deliberately
// separate re-declaration, not a duplicate).
func mustResolverTB(tb testing.TB, rules []guardrail.Rule) *guardrail.Resolver {
	tb.Helper()
	r, err := guardrail.NewResolver(rules, "default")
	if err != nil {
		tb.Fatal(err)
	}
	return r
}

// newRefreshBenchEngine builds an Engine over nWorktrees fake worktrees in
// one fake repo (a real directory holding just an empty `.git` directory —
// all discovery.DiscoverRepos needs, per its own doc comment: no real git
// repository required) and a stub Backend returning an nFilesPerWT-file
// canned diff for every one of them. Worktree paths are never created on
// disk: nothing on this path stats them except detectAgent, which tolerates
// a missing directory. Returns the engine and the worktree ids
// (worktreeID(path), the same derivation Refresh uses internally).
func newRefreshBenchEngine(tb testing.TB, nWorktrees, nFilesPerWT int) (*Engine, []string) {
	tb.Helper()
	root := tb.TempDir()
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repoPath, ".git"), 0o755); err != nil {
		tb.Fatal(err)
	}

	refs := make([]gitbackend.WorktreeRef, nWorktrees)
	ids := make([]string, nWorktrees)
	for i := range refs {
		p := filepath.Join(root, fmt.Sprintf("fake-wt-%03d", i))
		refs[i] = gitbackend.WorktreeRef{Path: p, Branch: "feature"}
		ids[i] = worktreeID(p)
	}
	be := &stubBackend{worktrees: refs, diffText: cannedDiffText(nFilesPerWT)}

	reg := registry.New()
	st, err := store.OpenSQLite(filepath.Join(root, "state.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if c, ok := st.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})
	gr := mustResolverTB(tb, guardrail.DefaultRules())
	e := New(Config{Roots: []string{root}, MaxDepth: 4, ActivityWindow: 30 * time.Second}, be, reg, st, gr)
	return e, ids
}

// primeMatchingReviews pre-populates review marks whose paths exactly match
// cannedDiffText's file paths, for every id — so pruneReviews' path-
// membership check (§refreshWorktree) finds every stored mark still present
// in the diff and issues zero deletes, a stable steady state across
// repeated Refresh calls. This mirrors the realistic common case (a
// long-running daemon's watcher loop polling a worktree whose diff hasn't
// changed since it was last reviewed) far more than a one-shot "prune
// everything" scenario would, and — unlike ClearWorktree/Unreview above —
// pruneReviews returns entirely early when a worktree's review map is
// already empty, so an unmatched-prime-then-drained dataset would silently
// stop exercising ReviewedFiles at all from the second call onward.
func primeMatchingReviews(tb testing.TB, e *Engine, ids []string, nFiles int) {
	tb.Helper()
	for _, id := range ids {
		for f := 0; f < nFiles; f++ {
			if err := e.st.SetReviewed(id, fmt.Sprintf("pkg/file%03d.go", f), "primed"); err != nil {
				tb.Fatal(err)
			}
		}
	}
}

// ---- benchmarks (§6.2) — trend/profiling via `go test -bench=. -benchmem` ----

// BenchmarkRefreshStoreOverhead100Worktrees measures one full Refresh's
// engine+store cost with git subprocess time excluded (P6-design.md §6.2): a
// stub Backend isolates everything Refresh does per worktree that ISN'T a
// git call — pruneReviews' and buildWorktree's ReviewedFiles reads (2 calls
// x 100 worktrees, matching §5's own "~200 ReviewedFiles prefix scans"
// framing for a full refresh) plus diffparse/hash/guardrail-eval, across 100
// worktrees in one repo. One untimed priming Refresh (matching every other
// benchmark's warm-cache convention) makes the first timed iteration behave
// identically to the rest — no first-time diff.ready publish or
// firstScanDone transition bleeding into the measurement. Budget: < 10 ms
// aggregate.
func BenchmarkRefreshStoreOverhead100Worktrees(b *testing.B) {
	const nWorktrees, nFiles = 100, 10
	e, ids := newRefreshBenchEngine(b, nWorktrees, nFiles)
	primeMatchingReviews(b, e, ids, nFiles)
	if err := e.Refresh(context.Background()); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := e.Refresh(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

// newSingleWorktreeBenchEngine builds an Engine over ONE fake worktree whose
// cached diff has nFiles files (via one priming Refresh against a stub
// Backend), with roughly half marked reviewed — a realistic partially-
// reviewed diff rather than an all-or-nothing extreme. Returns the engine
// and the worktree id.
func newSingleWorktreeBenchEngine(tb testing.TB, nFiles int) (*Engine, string) {
	tb.Helper()
	e, ids := newRefreshBenchEngine(tb, 1, nFiles)
	if err := e.Refresh(context.Background()); err != nil {
		tb.Fatal(err)
	}
	id := ids[0]

	d, ok := e.Diff(id)
	if !ok {
		tb.Fatal("worktree not cached after priming Refresh")
	}
	for i, f := range d.Files {
		if i%2 == 0 {
			if err := e.SetReviewed(id, f.Path, true, f.Hash); err != nil {
				tb.Fatal(err)
			}
		}
	}
	return e, id
}

// BenchmarkReviewedMap500FileDiff measures ReviewedMap's per-call cost
// against a single worktree whose cached diff has 500 files (P6-design.md
// §6.2). Budget: < 2 ms.
func BenchmarkReviewedMap500FileDiff(b *testing.B) {
	e, id := newSingleWorktreeBenchEngine(b, 500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := e.ReviewedMap(id); !ok {
			b.Fatal("worktree missing")
		}
	}
}

// ---- 10x-budget smoke gates (§6.3 layer 2) — WT_BENCH_GATE only ----

// requireBenchGate skips t unless WT_BENCH_GATE is set: these smoke gates
// are meant to run ONLY in the CI bench job (P6-design.md §6.3 layer 2) — a
// plain `go test ./...` must never execute them, so a bench hiccup or
// shared-runner variance can never flake the core suite or block a merge.
func requireBenchGate(t *testing.T) {
	t.Helper()
	if os.Getenv("WT_BENCH_GATE") == "" {
		t.Skip("WT_BENCH_GATE not set; skipping perf budget smoke gate (see P6-design.md §6.3)")
	}
}

// assertUnderBudget fails t if elapsed exceeds 10x budget: generous enough
// to never flake on shared-runner variance, tight enough to catch a lost
// index or an accidental O(n) scan the day it lands (P6-design.md §6.3).
func assertUnderBudget(t *testing.T, name string, elapsed, budget time.Duration) {
	t.Helper()
	gate := 10 * budget
	t.Logf("%s: %v (budget %v, 10x gate %v)", name, elapsed, budget, gate)
	if elapsed > gate {
		t.Errorf("%s took %v, want < %v (10x its %v budget)", name, elapsed, gate, budget)
	}
}

func TestBudgetRefreshStoreOverhead100Worktrees(t *testing.T) {
	requireBenchGate(t)
	const nWorktrees, nFiles = 100, 10
	e, ids := newRefreshBenchEngine(t, nWorktrees, nFiles)
	primeMatchingReviews(t, e, ids, nFiles)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err := e.Refresh(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	assertUnderBudget(t, "Refresh store-overhead (100 worktrees)", elapsed, 10*time.Millisecond)
}

func TestBudgetReviewedMap500FileDiff(t *testing.T) {
	requireBenchGate(t)
	e, id := newSingleWorktreeBenchEngine(t, 500)
	start := time.Now()
	_, ok := e.ReviewedMap(id)
	elapsed := time.Since(start)
	if !ok {
		t.Fatal("worktree missing")
	}
	assertUnderBudget(t, "ReviewedMap(500-file diff)", elapsed, 2*time.Millisecond)
}
