package guardrail

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// Pack is a per-repo rule-pack file: a checked-in .wtcockpit.toml at a repo's
// root (P5-design.md §1.3). Rules use the exact same shape (and TOML tags) as
// config.Config's own [[rules]] table.
type Pack struct {
	DisableRules []string `toml:"disable_rules"`
	Rules        []Rule   `toml:"rules"`
}

// ParsePack decodes pack TOML bytes strictly: malformed TOML or an
// unrecognised key (top-level or inside a [[rules]] entry) is an error —
// mirroring config.Load's own "a typo in a hand-edited guardrails file is a
// real footgun otherwise" philosophy. The caller (Resolver) is what turns a
// parse error into the fail-closed-to-global behaviour; ParsePack itself just
// reports it.
func ParsePack(data []byte) (Pack, error) {
	var p Pack
	meta, err := toml.Decode(string(data), &p)
	if err != nil {
		return Pack{}, err
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return Pack{}, fmt.Errorf("unknown pack key %q", undecoded[0])
	}
	return p, nil
}

// RuleWithSource is one entry of an Effective payload: a Rule plus where it
// came from.
type RuleWithSource struct {
	Rule
	Source string `json:"source"` // "default" | "global" | "pack"
}

// Effective is GET /api/rules's payload and `wt rules --json`'s frozen
// output: the fully-resolved rule set for one worktree's owning repo, with
// enough pack metadata to make precedence confusion debuggable in one
// command (P5-design.md §1.3).
type Effective struct {
	WorktreeID string           `json:"worktreeId"`
	RepoPath   string           `json:"repoPath"`
	PackPath   string           `json:"packPath"`   // "" when no pack file is present
	PackStatus string           `json:"packStatus"` // "none" | "ok" | "error: <msg>"
	Rules      []RuleWithSource `json:"rules"`
}

// Merge computes the effective, provenance-tagged rule list for one repo:
// global rules (tagged globalSource — "default" or "global") minus any name
// in pack.DisableRules, plus pack.Rules appended — a pack rule whose name
// matches a surviving global rule REPLACES it in place, tagged "pack" (the
// frozen precedence, P5-design.md §1.3). Pure: no I/O, no validation — the
// caller Compiles the resulting rules (via RuleWithSource.Rule) separately,
// which is what catches a pack rule that's individually fine but breaks the
// combined set (e.g. a duplicate name against a rule Merge didn't replace).
func Merge(global []Rule, globalSource string, pack Pack) []RuleWithSource {
	disabled := make(map[string]bool, len(pack.DisableRules))
	for _, n := range pack.DisableRules {
		disabled[n] = true
	}

	out := make([]RuleWithSource, 0, len(global)+len(pack.Rules))
	index := make(map[string]int, len(global)) // rule name -> its index in out
	for _, r := range global {
		if disabled[r.Name] {
			continue
		}
		index[r.Name] = len(out)
		out = append(out, RuleWithSource{Rule: r, Source: globalSource})
	}
	for _, r := range pack.Rules {
		if i, ok := index[r.Name]; ok {
			out[i] = RuleWithSource{Rule: r, Source: "pack"}
			continue
		}
		index[r.Name] = len(out)
		out = append(out, RuleWithSource{Rule: r, Source: "pack"})
	}
	return out
}
