package guardrail

import (
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

func sampleDiff() model.Diff {
	return model.Diff{
		WorktreeID: "wt1",
		Files: []model.DiffFile{
			{Path: "migrations/014_drop.sql", Status: model.FileModified, Stats: model.Stats{Add: 2, Del: 96}},
			{Path: "src/app.ts", Status: model.FileModified, Stats: model.Stats{Add: 40, Del: 4}},
			{Path: ".github/workflows/ci.yml", Status: model.FileModified, Stats: model.Stats{Add: 1, Del: 1}},
		},
	}
}

func TestPathGlobRuleTrips(t *testing.T) {
	rules := []Rule{
		{Name: "touches-migrations", Severity: "danger", PathGlob: "migrations/**", Message: "touches migrations"},
	}
	e := New(rules)
	hits := e.Eval(sampleDiff())
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d: %+v", len(hits), hits)
	}
	if hits[0].Rule != "touches-migrations" || hits[0].File != "migrations/014_drop.sql" {
		t.Errorf("unexpected hit: %+v", hits[0])
	}
	if hits[0].Severity != "danger" {
		t.Errorf("severity = %q", hits[0].Severity)
	}
}

func TestGlobMatchesNestedAndFlat(t *testing.T) {
	// ".github/workflows/*" should match the ci.yml file.
	e := New([]Rule{{Name: "ci", Severity: "warn", PathGlob: ".github/workflows/*", Message: "edits CI"}})
	hits := e.Eval(sampleDiff())
	if len(hits) != 1 || hits[0].File != ".github/workflows/ci.yml" {
		t.Fatalf("glob match failed: %+v", hits)
	}
}

func TestNetDeletionThresholdPerFile(t *testing.T) {
	// Rule: warn when a single file deletes >= 50 lines net.
	e := New([]Rule{{Name: "big-delete", Severity: "warn", MinNetDeleted: 50, Message: "large deletion"}})
	hits := e.Eval(sampleDiff())
	if len(hits) != 1 || hits[0].File != "migrations/014_drop.sql" {
		t.Fatalf("want big-delete on migrations file, got %+v", hits)
	}
}

func TestDeletionRatioWorktreeWide(t *testing.T) {
	// Rule: warn when the whole worktree deletes >= 2x what it adds.
	// sample totals: add=43, del=101 -> ratio ~2.35 -> trips.
	e := New([]Rule{{Name: "net-negative", Severity: "warn", MinDeleteAddRatio: 2.0, Message: "deletes >2x additions"}})
	hits := e.Eval(sampleDiff())
	found := false
	for _, h := range hits {
		if h.Rule == "net-negative" && h.File == "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected worktree-wide net-negative hit, got %+v", hits)
	}
}

func TestNoFalsePositives(t *testing.T) {
	clean := model.Diff{Files: []model.DiffFile{
		{Path: "src/util.go", Stats: model.Stats{Add: 5, Del: 1}},
	}}
	e := New([]Rule{
		{Name: "touches-migrations", Severity: "danger", PathGlob: "migrations/**"},
		{Name: "big-delete", Severity: "warn", MinNetDeleted: 50},
		{Name: "net-negative", Severity: "warn", MinDeleteAddRatio: 2.0},
	})
	if hits := e.Eval(clean); len(hits) != 0 {
		t.Fatalf("clean diff should trip nothing, got %+v", hits)
	}
}

// TestRuleWithNoConditionsNeverMatches covers a rule loaded from a config
// [[rules]] table with only "name" set (every condition field left at its
// TOML zero value): fileMatches' hasCond guard means such a rule matches no
// file, and it's not a ratio rule either (MinDeleteAddRatio == 0), so it must
// produce zero hits — never a crash, and never a false positive on every file.
func TestRuleWithNoConditionsNeverMatches(t *testing.T) {
	e := New([]Rule{{Name: "only-a-name"}})
	if hits := e.Eval(sampleDiff()); len(hits) != 0 {
		t.Fatalf("a rule with no conditions should never match, got %+v", hits)
	}
}

func TestDefaultRulesLoad(t *testing.T) {
	e := New(DefaultRules())
	hits := e.Eval(sampleDiff())
	if len(hits) == 0 {
		t.Fatal("default rules should catch the migrations + big-delete case")
	}
}
