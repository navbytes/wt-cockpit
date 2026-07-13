// bench_test.go is the §6.1 performance suite (P6-design.md §6.1, §6.3,
// WP3): testing.B benchmarks for every store operation the design budgets,
// plus 10×-budget smoke gates that run ONLY when WT_BENCH_GATE is set — a
// plain `go test ./...` must skip every TestBudget* test here (§6.3 layer
// 2). The startup-open budget for a "year-old", ~100k-row db (§6.2's own
// table, "how isolated: store benchmark") lives here too: it's the same
// OpenSQLite call under a bigger dataset, not an engine-level concern.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// §6.1's dataset: 100 worktrees × 200 reviewed files (20k rows) + 10k
// comments with 1 KiB bodies. datasetHotWT ("wt000") carries exactly 200 of
// those comments — the "200 comments of one worktree" row's fixture — while
// also being an ordinary 200-file worktree for the ReviewedFiles row.
const (
	datasetWorktrees   = 100
	datasetFilesPerWT  = 200
	datasetComments    = 10000
	datasetHotComments = 200
)

// startupDataset is §6.2's "year-old db (~100k rows)" startup-open budget's
// fixture: bigger than §6.1's, same shape (50k reviewed_files + 50k comments).
const (
	startupWorktrees  = 100
	startupFilesPerWT = 500
	startupComments   = 50000
)

// newBenchDataset builds a store fixture directly against the store's own
// prepared statements inside one transaction — the same technique
// sqliteStore.importPersisted uses, bypassing AddComment's per-worktree cap
// and per-call transaction — fast enough to redo on every benchmark/gate
// invocation (b.N-attempt reruns, -count=6, or a plain `go test` run of a
// gate) without dominating wall-clock time. tb is testing.TB so both
// *testing.B (trend benchmarks) and *testing.T (budget gates) share one
// fixture builder. Returns the open store, its db path, and the "hot"
// worktree id (always "wt000") that carries exactly 200 comments (capped at
// totalComments if that's smaller).
func newBenchDataset(tb testing.TB, nWorktrees, filesPerWT, totalComments int) (s *sqliteStore, path, hotWT string) {
	tb.Helper()
	path = filepath.Join(tb.TempDir(), "bench.db")
	st, err := OpenSQLite(path)
	if err != nil {
		tb.Fatal(err)
	}
	s = st.(*sqliteStore)
	tb.Cleanup(func() { _ = s.Close() })

	tx, err := s.db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	defer tx.Rollback() // no-op once Commit below succeeds

	setReviewed := tx.Stmt(s.setReviewedStmt)
	insertComment := tx.Stmt(s.insertCommentStmt)

	for w := 0; w < nWorktrees; w++ {
		wt := fmt.Sprintf("wt%03d", w)
		for f := 0; f < filesPerWT; f++ {
			if _, err := setReviewed.Exec(wt, fmt.Sprintf("file%03d.go", f), "hash"); err != nil {
				tb.Fatal(err)
			}
		}
	}

	body := strings.Repeat("x", 1024) // 1 KiB body, per §6.1's dataset note
	at := time.Now().Format(time.RFC3339Nano)
	seq := 0
	addN := func(wt string, n int) {
		for i := 0; i < n; i++ {
			seq++
			if _, err := insertComment.Exec(fmt.Sprintf("c-%d", seq), wt, "file000.go", 1, "new", body, "bench", "open", "hash", at); err != nil {
				tb.Fatal(err)
			}
		}
	}

	hotWT = fmt.Sprintf("wt%03d", 0)
	hotComments := datasetHotComments
	if totalComments < hotComments {
		hotComments = totalComments
	}
	addN(hotWT, hotComments)

	remaining, others := totalComments-hotComments, nWorktrees-1
	var base, extra int
	if others > 0 {
		base, extra = remaining/others, remaining%others
	}
	for w := 1; w < nWorktrees; w++ {
		n := base
		if w == nWorktrees-1 {
			n += extra
		}
		addN(fmt.Sprintf("wt%03d", w), n)
	}

	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return s, path, hotWT
}

// buildFiveMBStateJSON builds a legacy state.json shape (open.go's persisted
// struct) sized to ~5 MiB via one worktree's worth of 1 KiB-body comments —
// §6.1's "one-time import" dataset. The per-comment marshaled size is
// measured once so the target is hit without repeatedly re-marshaling an
// ever-growing structure.
func buildFiveMBStateJSON(tb testing.TB) []byte {
	tb.Helper()
	const target = 5 * 1024 * 1024
	body := strings.Repeat("z", 1024)
	at := time.Now()
	sample := model.Comment{ID: "c-0", WorktreeID: "wt0", File: "file000.go", Line: 1, Side: "new", Body: body, Author: "bench", State: "open", FileHash: "hash", At: at}
	sb, err := json.Marshal(sample)
	if err != nil {
		tb.Fatal(err)
	}
	n := target/(len(sb)+1) + 1 // +1 per entry for the array comma; +1 overall to clear the target

	p := persisted{ReviewedFiles: map[string]map[string]string{"wt0": {}}, Comments: map[string][]model.Comment{}}
	for f := 0; f < 200; f++ {
		p.ReviewedFiles["wt0"][fmt.Sprintf("file%03d.go", f)] = "hash"
	}
	comments := make([]model.Comment, n)
	for i := range comments {
		comments[i] = model.Comment{ID: fmt.Sprintf("c-%d", i), WorktreeID: "wt0", File: "file000.go", Line: 1, Side: "new", Body: body, Author: "bench", State: "open", FileHash: "hash", At: at}
	}
	p.Comments["wt0"] = comments

	raw, err := json.Marshal(p)
	if err != nil {
		tb.Fatal(err)
	}
	return raw
}

// ---- benchmarks (§6.1/§6.2) — trend/profiling via `go test -bench=. -benchmem` ----

func BenchmarkSetReviewedUpsert(b *testing.B) {
	s, _, hotWT := newBenchDataset(b, datasetWorktrees, datasetFilesPerWT, datasetComments)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// ponytail: same (worktree, file) key every iteration — always hits
		// the ON CONFLICT DO UPDATE branch (the true "upsert" cost this
		// budget targets), and stays valid for any b.N with no repopulation.
		if err := s.SetReviewed(hotWT, "bench-upsert.go", fmt.Sprintf("hash-%d", i)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnreview(b *testing.B) {
	s, _, hotWT := newBenchDataset(b, datasetWorktrees, datasetFilesPerWT, datasetComments)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// ponytail: repeats the same key (a real delete once, a point-miss
		// after — see BenchmarkClearWorktree's note below: the regression
		// this budget actually guards against, a missing index forcing a
		// full-table scan, costs the same either way), so no per-iteration
		// repopulation is needed to keep this a meaningful regression guard.
		if err := s.Unreview(hotWT, "file000.go"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReviewedFiles200Files(b *testing.B) {
	s, _, hotWT := newBenchDataset(b, datasetWorktrees, datasetFilesPerWT, datasetComments)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.ReviewedFiles(hotWT); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAddComment1KiB(b *testing.B) {
	s, _, _ := newBenchDataset(b, datasetWorktrees, datasetFilesPerWT, datasetComments)
	body := strings.Repeat("y", 1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Bucketed worktree IDs, distinct from the shared dataset's wt000..
		// wt099: unlike the point ops above, AddComment enforces a real
		// per-worktree cap (maxCommentsPerWorktree=500), so sustained
		// inserts genuinely need fresh capacity, not just a fresh key.
		wt := fmt.Sprintf("addc-%d", i%1000)
		c := model.Comment{ID: fmt.Sprintf("bench-c-%d", i), WorktreeID: wt, File: "f.go", Body: body, Author: "bench"}
		if err := s.AddComment(c); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkComments200Comments(b *testing.B) {
	s, _, hotWT := newBenchDataset(b, datasetWorktrees, datasetFilesPerWT, datasetComments)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Comments(hotWT); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkClearWorktree(b *testing.B) {
	s, _, hotWT := newBenchDataset(b, datasetWorktrees, datasetFilesPerWT, datasetComments)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// ponytail: clears the same worktree repeatedly (a real 200+200-row
		// delete once, 0-row deletes after). The regression this budget
		// guards against — a missing index forcing a full-table scan —
		// costs the same either way (SQLite's planner cost is a function of
		// table size and index presence, not how many rows a given key
		// matches), so no per-iteration repopulation is needed.
		if err := s.ClearWorktree(hotWT); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpenExistingDB(b *testing.B) {
	s, path, _ := newBenchDataset(b, datasetWorktrees, datasetFilesPerWT, datasetComments)
	if err := s.Close(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := OpenSQLite(path)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_ = st.(*sqliteStore).Close()
		b.StartTimer()
	}
}

// BenchmarkStartupOpen100kRows is §6.2's "daemon store.Open incl. migration
// check on a year-old db (~100k rows)" row — same operation as
// BenchmarkOpenExistingDB, bigger dataset.
func BenchmarkStartupOpen100kRows(b *testing.B) {
	s, path, _ := newBenchDataset(b, startupWorktrees, startupFilesPerWT, startupComments)
	if err := s.Close(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := OpenSQLite(path)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_ = st.(*sqliteStore).Close()
		b.StartTimer()
	}
}

func BenchmarkImport5MBJSON(b *testing.B) {
	raw := buildFiveMBStateJSON(b)
	dir := b.TempDir()
	jsonPath := filepath.Join(dir, "state.json")
	dbPath := filepath.Join(dir, "state.db")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if err := os.WriteFile(jsonPath, raw, 0o644); err != nil {
			b.Fatal(err)
		}
		os.Remove(dbPath)
		os.Remove(dbPath + "-wal")
		os.Remove(dbPath + "-shm")
		b.StartTimer()

		st, err := Open(dbPath) // store.Open, not OpenSQLite: import only fires through the dispatch layer
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_ = st.(*sqliteStore).Close()
		b.StartTimer()
	}
}

// TestMemoryFootprintArtifact reports the store layer's heap growth for the
// §6.1 dataset (§6.2's memory row: "runtime.MemStats snapshot in the bench
// job; artifact, not a gate") — logged, never asserted; gated behind
// WT_BENCH_GATE purely to keep this measurement (and the GC pause it forces)
// out of every contributor's normal `go test ./...`, matching "in the bench
// job" — not because it's a pass/fail gate.
func TestMemoryFootprintArtifact(t *testing.T) {
	requireBenchGate(t)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	s, _, _ := newBenchDataset(t, datasetWorktrees, datasetFilesPerWT, datasetComments)

	runtime.GC()
	runtime.ReadMemStats(&after)
	_ = s.Close()

	// Signed subtraction (HeapAlloc is uint64): background GC noise between
	// the two snapshots can occasionally make "after" smaller than "before"
	// on an already-warm test binary, which would otherwise underflow into
	// a nonsense huge number rather than a small/negative one.
	delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	deltaMB := float64(delta) / (1024 * 1024)
	t.Logf("store layer heap growth for the §6.1 dataset (100 wt x 200 files + 10k comments, 1 KiB bodies): %.2f MB (budget < 25 MB, artifact only)", deltaMB)
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

func TestBudgetSetReviewed(t *testing.T) {
	requireBenchGate(t)
	s, _, hotWT := newBenchDataset(t, datasetWorktrees, datasetFilesPerWT, datasetComments)
	start := time.Now()
	err := s.SetReviewed(hotWT, "gate-upsert.go", "h")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	assertUnderBudget(t, "SetReviewed", elapsed, 2*time.Millisecond)
}

func TestBudgetUnreview(t *testing.T) {
	requireBenchGate(t)
	s, _, hotWT := newBenchDataset(t, datasetWorktrees, datasetFilesPerWT, datasetComments)
	start := time.Now()
	err := s.Unreview(hotWT, "file000.go")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	assertUnderBudget(t, "Unreview", elapsed, 2*time.Millisecond)
}

func TestBudgetReviewedFiles200Files(t *testing.T) {
	requireBenchGate(t)
	s, _, hotWT := newBenchDataset(t, datasetWorktrees, datasetFilesPerWT, datasetComments)
	start := time.Now()
	_, err := s.ReviewedFiles(hotWT)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	assertUnderBudget(t, "ReviewedFiles(200 files)", elapsed, time.Millisecond)
}

func TestBudgetAddComment1KiB(t *testing.T) {
	requireBenchGate(t)
	s, _, _ := newBenchDataset(t, datasetWorktrees, datasetFilesPerWT, datasetComments)
	c := model.Comment{ID: "gate-c", WorktreeID: "addc-gate", File: "f.go", Body: strings.Repeat("y", 1024), Author: "bench"}
	start := time.Now()
	err := s.AddComment(c)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	assertUnderBudget(t, "AddComment(1 KiB)", elapsed, 2*time.Millisecond)
}

func TestBudgetComments200Comments(t *testing.T) {
	requireBenchGate(t)
	s, _, hotWT := newBenchDataset(t, datasetWorktrees, datasetFilesPerWT, datasetComments)
	start := time.Now()
	_, err := s.Comments(hotWT)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	assertUnderBudget(t, "Comments(200 comments)", elapsed, 5*time.Millisecond)
}

func TestBudgetClearWorktree(t *testing.T) {
	requireBenchGate(t)
	s, _, hotWT := newBenchDataset(t, datasetWorktrees, datasetFilesPerWT, datasetComments)
	start := time.Now()
	err := s.ClearWorktree(hotWT)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	assertUnderBudget(t, "ClearWorktree(200+200 rows)", elapsed, 5*time.Millisecond)
}

func TestBudgetOpenExistingDB(t *testing.T) {
	requireBenchGate(t)
	s, path, _ := newBenchDataset(t, datasetWorktrees, datasetFilesPerWT, datasetComments)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	st, err := OpenSQLite(path)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	defer st.(*sqliteStore).Close()
	assertUnderBudget(t, "Open existing db (20k+10k rows)", elapsed, 100*time.Millisecond)
}

func TestBudgetStartupOpen100kRows(t *testing.T) {
	requireBenchGate(t)
	s, path, _ := newBenchDataset(t, startupWorktrees, startupFilesPerWT, startupComments)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	st, err := OpenSQLite(path)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	defer st.(*sqliteStore).Close()
	assertUnderBudget(t, "Startup open (~100k rows)", elapsed, 150*time.Millisecond)
}

func TestBudgetImport5MBJSON(t *testing.T) {
	requireBenchGate(t)
	raw := buildFiveMBStateJSON(t)
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "state.json")
	dbPath := filepath.Join(dir, "state.db")
	if err := os.WriteFile(jsonPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	st, err := Open(dbPath)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	defer st.(*sqliteStore).Close()
	assertUnderBudget(t, "One-time import of ~5 MB state.json", elapsed, time.Second)
}
