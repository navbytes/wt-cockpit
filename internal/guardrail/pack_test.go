package guardrail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParsePackValidFixture(t *testing.T) {
	p, err := ParsePack(readTestdata(t, "pack_valid.toml"))
	if err != nil {
		t.Fatalf("ParsePack(pack_valid.toml): %v", err)
	}
	if len(p.DisableRules) != 1 || p.DisableRules[0] != "net-negative" {
		t.Errorf("DisableRules = %+v, want [net-negative]", p.DisableRules)
	}
	if len(p.Rules) != 2 {
		t.Fatalf("Rules = %+v, want 2 entries", p.Rules)
	}
	if p.Rules[0].Name != "touches-payments" || p.Rules[0].Severity != "danger" {
		t.Errorf("Rules[0] = %+v", p.Rules[0])
	}
	if p.Rules[1].Name != "large-deletion" || p.Rules[1].MinNetDeleted != 200 {
		t.Errorf("Rules[1] = %+v", p.Rules[1])
	}
}

func TestParsePackMalformedTOMLErrors(t *testing.T) {
	_, err := ParsePack(readTestdata(t, "pack_malformed_toml.toml"))
	if err == nil {
		t.Fatal("expected an error for malformed TOML")
	}
}

func TestParsePackUnknownTopLevelKeyErrorsAndNamesIt(t *testing.T) {
	_, err := ParsePack(readTestdata(t, "pack_unknown_key.toml"))
	if err == nil {
		t.Fatal("expected an error for an unrecognised top-level key")
	}
	if !strings.Contains(err.Error(), "dissable_rules") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestParsePackUnknownRuleFieldErrorsAndNamesIt(t *testing.T) {
	_, err := ParsePack(readTestdata(t, "pack_unknown_rule_field.toml"))
	if err == nil {
		t.Fatal("expected an error for a mistyped [[rules]] field")
	}
	if !strings.Contains(err.Error(), "path_glbo") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

// TestParsePackFailsCompileFixtureParsesButIsInvalid: this fixture is valid
// TOML with all-known keys — ParsePack itself must succeed; it's Compile (via
// Resolver, tested separately) that rejects the bad severity.
func TestParsePackFailsCompileFixtureParsesButIsInvalid(t *testing.T) {
	p, err := ParsePack(readTestdata(t, "pack_fails_compile.toml"))
	if err != nil {
		t.Fatalf("ParsePack should succeed (valid shape): %v", err)
	}
	if _, err := Compile([]Rule{p.Rules[0]}); err == nil {
		t.Fatal("precondition: the fixture's rule should fail Compile (bad severity)")
	}
}

func TestParsePackEmptyBytesIsAnEmptyPack(t *testing.T) {
	p, err := ParsePack(nil)
	if err != nil {
		t.Fatalf("empty pack bytes should not error, got: %v", err)
	}
	if len(p.DisableRules) != 0 || len(p.Rules) != 0 {
		t.Errorf("expected a zero-value Pack, got %+v", p)
	}
}

// ---- Merge (pure, provenance-tagging) ----

func TestMergeAppendsNewPackRulesTaggedPack(t *testing.T) {
	global := []Rule{{Name: "a", PathGlob: "x/**"}}
	pack := Pack{Rules: []Rule{{Name: "b", PathGlob: "y/**"}}}
	got := Merge(global, "default", pack)
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %+v", got)
	}
	if got[0].Name != "a" || got[0].Source != "default" {
		t.Errorf("got[0] = %+v, want name=a source=default", got[0])
	}
	if got[1].Name != "b" || got[1].Source != "pack" {
		t.Errorf("got[1] = %+v, want name=b source=pack", got[1])
	}
}

func TestMergeSameNamePackRuleReplacesGlobalInPlace(t *testing.T) {
	global := []Rule{
		{Name: "a", PathGlob: "x/**"},
		{Name: "large-deletion", MinNetDeleted: 80},
		{Name: "c", PathGlob: "z/**"},
	}
	pack := Pack{Rules: []Rule{{Name: "large-deletion", MinNetDeleted: 200}}}
	got := Merge(global, "default", pack)

	if len(got) != 3 {
		t.Fatalf("replace must not change the count, got %+v", got)
	}
	// Replaced IN PLACE: same position as the original "large-deletion".
	if got[1].Name != "large-deletion" || got[1].MinNetDeleted != 200 || got[1].Source != "pack" {
		t.Errorf("got[1] = %+v, want the pack's overriding definition in place, tagged pack", got[1])
	}
	if got[0].Name != "a" || got[2].Name != "c" {
		t.Errorf("surrounding rules must keep their position, got %+v", got)
	}
}

func TestMergeDisableRulesRemovesGlobalRule(t *testing.T) {
	global := []Rule{{Name: "a"}, {Name: "net-negative"}, {Name: "c"}}
	pack := Pack{DisableRules: []string{"net-negative"}}
	got := Merge(global, "global", pack)
	if len(got) != 2 {
		t.Fatalf("want 2 surviving rules, got %+v", got)
	}
	for _, r := range got {
		if r.Name == "net-negative" {
			t.Errorf("net-negative should have been disabled, got %+v", got)
		}
	}
}

// TestMergeDisableThenPackReintroducesSameName: disabling a global rule and
// then a pack rule using the SAME name should behave as an addition (no
// surviving global entry to replace), tagged "pack".
func TestMergeDisableThenPackReintroducesSameName(t *testing.T) {
	global := []Rule{{Name: "net-negative", MinDeleteAddRatio: 3.0}}
	pack := Pack{
		DisableRules: []string{"net-negative"},
		Rules:        []Rule{{Name: "net-negative", MinDeleteAddRatio: 10.0}},
	}
	got := Merge(global, "default", pack)
	if len(got) != 1 || got[0].Source != "pack" || got[0].MinDeleteAddRatio != 10.0 {
		t.Errorf("got = %+v, want a single pack-sourced net-negative at ratio 10.0", got)
	}
}

// TestMergePackWithDuplicateRuleNameSilentlyCollapsesToLastOne is the MINOR-2
// fix pin. When a pack's OWN [[rules]] list contains two entries sharing a
// name (a plausible copy-paste mistake, with no corresponding global rule to
// "replace"), Merge used to conflate "this name came from a global rule"
// with "this name was already added by an earlier PACK rule in this same
// loop" — so the second pack rule silently overwrote the first in place
// (out[i] = ...) instead of ever being seen as a duplicate, and by the time
// the caller (Resolver.loadPack) ran Compile on the merged result, only one
// rule survived, so Compile's own duplicate-name check never fired either.
// Merge now leaves a genuine intra-pack duplicate as a SECOND distinct
// entry (rather than overwriting the first), so Compile catches it exactly
// like the identical mistake in config.toml's [[rules]] list already does.
func TestMergePackWithDuplicateRuleNameSilentlyCollapsesToLastOne(t *testing.T) {
	pack := Pack{Rules: []Rule{
		{Name: "dup", Severity: "warn", PathGlob: "a/**"},
		{Name: "dup", Severity: "danger", PathGlob: "b/**"},
	}}
	merged := Merge(nil, "default", pack)
	// Expected (once fixed): Merge/Compile should surface this as an error
	// (mirroring Compile's own duplicate-name rejection for the global list),
	// not silently keep exactly one entry.
	rules := make([]Rule, len(merged))
	for i, m := range merged {
		rules[i] = m.Rule
	}
	if _, err := Compile(rules); err == nil {
		t.Fatalf("expected a duplicate-rule-name error for a pack with two [[rules]] entries named %q, got none (merged=%+v)", "dup", merged)
	}
}

func TestMergeEmptyPackReturnsGlobalUnchangedButTagged(t *testing.T) {
	global := []Rule{{Name: "a"}, {Name: "b"}}
	got := Merge(global, "global", Pack{})
	if len(got) != 2 {
		t.Fatalf("want the global set untouched, got %+v", got)
	}
	for _, r := range got {
		if r.Source != "global" {
			t.Errorf("expected every rule tagged %q, got %+v", "global", r)
		}
	}
}
