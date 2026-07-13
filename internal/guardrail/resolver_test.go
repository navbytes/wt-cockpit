package guardrail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

func mustResolver(t *testing.T, rules []Rule, source string) *Resolver {
	t.Helper()
	r, err := NewResolver(rules, source)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func writePack(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, packFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolverNoPackUsesGlobalOnly: a repo with no .wtcockpit.toml at all
// resolves straight to the global engine, packStatus "none", packPath "".
func TestResolverNoPackUsesGlobalOnly(t *testing.T) {
	repo := t.TempDir()
	r := mustResolver(t, []Rule{{Name: "a", PathGlob: "**"}}, "default")

	eff := r.Effective("wt1", repo)
	if eff.PackPath != "" || eff.PackStatus != "none" {
		t.Errorf("PackPath/PackStatus = %q/%q, want empty/none", eff.PackPath, eff.PackStatus)
	}
	if len(eff.Rules) != 1 || eff.Rules[0].Source != "default" {
		t.Errorf("Rules = %+v, want the one global rule tagged default", eff.Rules)
	}

	eng := r.For(repo)
	if eng != r.globalEngine {
		t.Error("For() with no pack should return the exact global engine instance")
	}
}

// TestResolverValidPackAddsReplacesAndDisables exercises the full precedence
// contract against the testdata/pack_valid.toml fixture: net-negative
// disabled, touches-payments added, large-deletion overridden in place.
func TestResolverValidPackAddsReplacesAndDisables(t *testing.T) {
	repo := t.TempDir()
	body := string(readTestdata(t, "pack_valid.toml"))
	writePack(t, repo, body)

	global := []Rule{
		{Name: "net-negative", MinDeleteAddRatio: 3.0},
		{Name: "large-deletion", MinNetDeleted: 80},
	}
	r := mustResolver(t, global, "default")

	eff := r.Effective("wt1", repo)
	if eff.PackStatus != "ok" {
		t.Fatalf("PackStatus = %q, want ok; rules=%+v", eff.PackStatus, eff.Rules)
	}
	if eff.PackPath == "" {
		t.Error("PackPath should be set once a pack is in effect")
	}

	byName := map[string]RuleWithSource{}
	for _, rr := range eff.Rules {
		byName[rr.Name] = rr
	}
	if _, ok := byName["net-negative"]; ok {
		t.Errorf("net-negative should be disabled by the pack, got %+v", eff.Rules)
	}
	tp, ok := byName["touches-payments"]
	if !ok || tp.Source != "pack" {
		t.Errorf("touches-payments should be added, tagged pack: %+v", byName)
	}
	ld, ok := byName["large-deletion"]
	if !ok || ld.Source != "pack" || ld.MinNetDeleted != 200 {
		t.Errorf("large-deletion should be replaced in place at 200, tagged pack: %+v", ld)
	}

	// The pack's threshold must actually be the one Eval uses.
	eng := r.For(repo)
	diff := model.Diff{Files: []model.DiffFile{{Path: "x.go", Stats: model.Stats{Add: 0, Del: 150}}}}
	if hits := eng.Eval(diff); len(hits) != 0 {
		t.Errorf("150 net-deleted should NOT trip the pack-overridden 200 threshold, got %+v", hits)
	}
	diff2 := model.Diff{Files: []model.DiffFile{{Path: "x.go", Stats: model.Stats{Add: 0, Del: 250}}}}
	if hits := eng.Eval(diff2); len(hits) != 1 {
		t.Errorf("250 net-deleted should trip the pack-overridden 200 threshold, got %+v", hits)
	}
}

// TestResolverMalformedPackFailsClosedToGlobal covers every malformed-pack
// flavour (bad TOML, unknown key, a rule that fails Compile): the resolver
// must fall back to the global engine/rules rather than ever disabling
// guardrails, and packStatus must report the error.
func TestResolverMalformedPackFailsClosedToGlobal(t *testing.T) {
	fixtures := []string{"pack_malformed_toml.toml", "pack_unknown_key.toml", "pack_unknown_rule_field.toml", "pack_fails_compile.toml"}
	global := []Rule{{Name: "large-deletion", MinNetDeleted: 80}}

	for _, fx := range fixtures {
		t.Run(fx, func(t *testing.T) {
			repo := t.TempDir()
			writePack(t, repo, string(readTestdata(t, fx)))
			r := mustResolver(t, global, "default")

			eff := r.Effective("wt1", repo)
			if !strings.HasPrefix(eff.PackStatus, "error:") {
				t.Fatalf("PackStatus = %q, want an error: prefix for a malformed pack", eff.PackStatus)
			}
			if len(eff.Rules) != 1 || eff.Rules[0].Name != "large-deletion" || eff.Rules[0].Source != "default" {
				t.Errorf("a malformed pack must fall back to the global rule set untouched, got %+v", eff.Rules)
			}
			if r.For(repo) != r.globalEngine {
				t.Error("a malformed pack must fail closed to the exact global engine, never a half-applied one")
			}
		})
	}
}

// TestResolverCacheInvalidatesOnPackMtimeBump: after resolving a repo once,
// rewriting the pack (with a size change, so the mtime+size cache key is
// guaranteed to differ regardless of filesystem timestamp resolution) must
// be picked up on the next lookup — no restart or explicit invalidation call
// needed.
func TestResolverCacheInvalidatesOnPackMtimeBump(t *testing.T) {
	repo := t.TempDir()
	writePack(t, repo, `disable_rules = ["a"]`+"\n")
	r := mustResolver(t, []Rule{{Name: "a", PathGlob: "**"}, {Name: "b", PathGlob: "**"}}, "default")

	eff1 := r.Effective("wt1", repo)
	if len(eff1.Rules) != 1 || eff1.Rules[0].Name != "b" {
		t.Fatalf("initial resolve: Rules = %+v, want just [b]", eff1.Rules)
	}

	// Force a strictly later mtime in case the filesystem's clock resolution
	// is coarser than this test's wall-clock speed.
	future := time.Now().Add(2 * time.Second)
	writePack(t, repo, `disable_rules = ["a", "b"]`+"\n\n# padding to change size too\n")
	if err := os.Chtimes(filepath.Join(repo, packFileName), future, future); err != nil {
		t.Fatal(err)
	}

	eff2 := r.Effective("wt1", repo)
	if len(eff2.Rules) != 0 {
		t.Errorf("after the pack changed to disable both rules, Rules = %+v, want none", eff2.Rules)
	}
}

// TestResolverPackRemovedRevertsToGlobal: deleting a previously-valid pack
// must revert to the global rules on the next lookup, not keep serving a
// stale cached pack-engine forever.
func TestResolverPackRemovedRevertsToGlobal(t *testing.T) {
	repo := t.TempDir()
	writePack(t, repo, `disable_rules = ["a"]`+"\n")
	r := mustResolver(t, []Rule{{Name: "a", PathGlob: "**"}}, "default")

	if eff := r.Effective("wt1", repo); len(eff.Rules) != 0 {
		t.Fatalf("precondition: pack should disable rule a, got %+v", eff.Rules)
	}

	if err := os.Remove(filepath.Join(repo, packFileName)); err != nil {
		t.Fatal(err)
	}
	eff := r.Effective("wt1", repo)
	if eff.PackStatus != "none" || len(eff.Rules) != 1 {
		t.Errorf("after removing the pack, want packStatus=none and the global rule back, got %+v", eff)
	}
}

// TestResolverIsolatesRepos: two different repoPaths must resolve
// independently (one repo's pack must not leak into another's).
func TestResolverIsolatesRepos(t *testing.T) {
	repoA := t.TempDir()
	repoB := t.TempDir()
	writePack(t, repoA, `disable_rules = ["a"]`+"\n")
	r := mustResolver(t, []Rule{{Name: "a", PathGlob: "**"}}, "default")

	if eff := r.Effective("wt-a", repoA); len(eff.Rules) != 0 {
		t.Errorf("repoA should have rule a disabled, got %+v", eff.Rules)
	}
	if eff := r.Effective("wt-b", repoB); len(eff.Rules) != 1 {
		t.Errorf("repoB has no pack and must be unaffected by repoA's, got %+v", eff.Rules)
	}
}

// TestResolverStatsCountsLoadedAndErrored pins the statusPayload-facing
// aggregate: one repo with a valid pack, one with a malformed pack, one with
// none at all.
func TestResolverStatsCountsLoadedAndErrored(t *testing.T) {
	ok := t.TempDir()
	writePack(t, ok, `disable_rules = ["a"]`+"\n")
	bad := t.TempDir()
	writePack(t, bad, "not valid [ toml")
	none := t.TempDir()

	r := mustResolver(t, []Rule{{Name: "a", PathGlob: "**"}}, "default")
	r.Effective("w1", ok)
	r.Effective("w2", bad)
	r.Effective("w3", none)

	loaded, errs := r.Stats()
	if loaded != 1 {
		t.Errorf("loaded = %d, want 1", loaded)
	}
	if errs != 1 {
		t.Errorf("errors = %d, want 1", errs)
	}
}

// TestNewResolverRejectsInvalidGlobalRules: NewResolver is a defensive
// Compile wrapper — an invalid global rule set must error, not panic or
// silently drop the bad rule.
func TestNewResolverRejectsInvalidGlobalRules(t *testing.T) {
	if _, err := NewResolver([]Rule{{Name: "bad", Severity: "critical"}}, "default"); err == nil {
		t.Fatal("expected an error constructing a Resolver from an invalid global rule set")
	}
}
