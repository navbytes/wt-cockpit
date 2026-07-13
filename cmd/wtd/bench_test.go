// bench_test.go benchmarks GET /api/worktrees over a 100-worktree registry
// snapshot (P6-design.md §6.2: "Serving the radar ... registry snapshot;
// store not on this path" — handleWorktrees only ever calls s.eng.List(),
// so a bare *engine.Engine wired to a populated *registry.Registry, with no
// real git/store behind it, is a faithful, isolated fixture), plus the
// matching 10×-budget smoke gate (WT_BENCH_GATE only, §6.3 layer 2).
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/engine"
	"github.com/navbytes/wt-cockpit/internal/model"
	"github.com/navbytes/wt-cockpit/internal/registry"
)

// newRadarBenchServer builds a *server whose engine has n worktrees already
// upserted straight into its registry. handleWorktrees never calls be/st/gr
// (only s.eng.List(), which is a pure registry read), so nil stand-ins for
// engine.New's other dependencies are enough — no real git repo or store
// needed to exercise this handler in isolation.
func newRadarBenchServer(n int) *server {
	reg := registry.New()
	for i := 0; i < n; i++ {
		reg.Upsert(model.Worktree{
			ID:         fmt.Sprintf("wt%03d", i),
			Repo:       "repo",
			Name:       fmt.Sprintf("wt%03d", i),
			Path:       fmt.Sprintf("/fake/wt%03d", i),
			Branch:     "feature",
			Base:       "main",
			Stats:      model.Stats{Add: 10, Del: 2, Files: 5},
			LastChange: time.Now(),
		})
	}
	eng := engine.New(engine.Config{}, nil, reg, nil, nil)
	return &server{eng: eng}
}

// BenchmarkHandleWorktrees100 measures GET /api/worktrees over a 100-worktree
// registry snapshot (P6-design.md §6.2). Budget: < 5 ms.
func BenchmarkHandleWorktrees100(b *testing.B) {
	handler := newRadarBenchServer(100).routes()
	req := httptest.NewRequest(http.MethodGet, "/api/worktrees", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}

// ---- 10x-budget smoke gate (§6.3 layer 2) — WT_BENCH_GATE only ----

// requireBenchGate skips t unless WT_BENCH_GATE is set: this smoke gate is
// meant to run ONLY in the CI bench job (P6-design.md §6.3 layer 2) — a
// plain `go test ./...` must never execute it, so a bench hiccup or
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

func TestBudgetHandleWorktrees100(t *testing.T) {
	requireBenchGate(t)
	handler := newRadarBenchServer(100).routes()
	req := httptest.NewRequest(http.MethodGet, "/api/worktrees", nil)

	start := time.Now()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	assertUnderBudget(t, "GET /api/worktrees (100 worktrees)", elapsed, 5*time.Millisecond)
}
