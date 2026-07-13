// sqlite_store_test.go is WP1's drop-in proof at the engine level
// (P6-design.md WP1 / .claude/company/handoffs/P6-design.md): the SQLite
// store is a behind-the-seam swap for jsonStore, so the engine — built via
// the same engine.New every other test uses — must work against it with
// zero engine changes. Mirrors newEngine (engine_test.go) but for
// store.OpenSQLite, and drives a full comment + review + approve cycle
// against a real temp git repo using the package's existing fixtures
// (buildWorkspace/commitWorktree/testGitEnv/mustResolver/findByBranch/
// findCommentView).
package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/gitbackend"
	"github.com/navbytes/wt-cockpit/internal/guardrail"
	"github.com/navbytes/wt-cockpit/internal/registry"
	"github.com/navbytes/wt-cockpit/internal/store"
)

func TestEngineWorksAgainstSQLiteStore(t *testing.T) {
	root := buildWorkspace(t)
	commitWorktree(t, root) // Approve requires a clean, committed worktree

	reg := registry.New()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	be := gitbackend.NewCLIWithEnv(testGitEnv())
	gr := mustResolver(t, guardrail.DefaultRules())
	e := New(Config{
		Roots:          []string{root},
		MaxDepth:       4,
		ActivityWindow: 30 * time.Second,
	}, be, reg, st, gr)

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat := findByBranch(e.List(), "feature")
	if feat == nil {
		t.Fatal("feature worktree not discovered")
	}

	d, _ := e.Diff(feat.ID)
	if len(d.Files) == 0 {
		t.Fatal("precondition: feature diff must have files")
	}

	// A comment round-trips through the SQLite store.
	c, err := e.AddComment(feat.ID, d.Files[0].Path, 1, "", "looks good", "")
	if err != nil {
		t.Fatalf("AddComment against sqlite store: %v", err)
	}
	views, err := e.Comments(feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if findCommentView(views, c.ID) == nil {
		t.Fatalf("comment %s not found after AddComment: %+v", c.ID, views)
	}

	// Review every file, then approve — the engine's single gated write path.
	for _, f := range d.Files {
		if err := e.SetReviewed(feat.ID, f.Path, true, f.Hash); err != nil {
			t.Fatalf("SetReviewed %s against sqlite store: %v", f.Path, err)
		}
	}
	res, err := e.Approve(feat.ID)
	if err != nil {
		t.Fatalf("approve against sqlite store: %v", err)
	}
	if res.Merged != "feature" || res.Into != "main" {
		t.Errorf("unexpected approve result: %+v", res)
	}
	if findByBranch(e.List(), "feature") != nil {
		t.Error("approved worktree should be removed from the registry")
	}
}
