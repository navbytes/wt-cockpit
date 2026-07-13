// Package guardrail evaluates declarative rules over a worktree diff and returns
// the tripped rules. Rules are data (globs + thresholds + RE2 patterns), never
// code, so growing from 2 rules to 20 is a config change. Evaluation is pure and
// allocation-light.
//
// THE NON-ECHO INVARIANT (security, frozen — P5-design.md §1.1): no hit
// Message, default or computed, ever contains matched line content or the
// matched token. Content rules (added_pattern, entropy) report the rule's
// message plus file + line + a match COUNT only. This is what keeps secrets
// out of the SSE stream, the JSON API, wtd's logs, and desktop notifications.
package guardrail

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// maxAddedLinesScanned caps how many added lines a content condition
// (added_pattern, entropy) scans per file.
//
// ponytail: constant cap; make it a config knob only if a real repo ever
// needs more.
const maxAddedLinesScanned = 5000

// Rule is one declarative guardrail. A rule may combine several per-file
// conditions (ANDed) — see hasFileCond — or exactly one worktree-wide class
// (ratio / files-changed / total-churn); mixing the two classes on one rule
// is a Compile error.
type Rule struct {
	Name     string `json:"name" toml:"name"`         // auto-filled "rule-<n>" when empty; duplicates = load error
	Severity string `json:"severity" toml:"severity"` // "" | "warn" | "danger" — validated at Compile
	Message  string `json:"message" toml:"message"`

	// Per-file conditions: all conditions a rule sets must hold for a file to
	// trip it. A rule whose only set field is ExcludeGlobs has no real
	// condition and so matches nothing (hasFileCond).
	PathGlob        string   `json:"pathGlob,omitempty" toml:"path_glob"`                // e.g. "migrations/**"; mutually exclusive with PathGlobs
	PathGlobs       []string `json:"pathGlobs,omitempty" toml:"path_globs"`              // any-of
	ExcludeGlobs    []string `json:"excludeGlobs,omitempty" toml:"exclude_globs"`        // exemption: a matching file skips this rule
	Status          string   `json:"status,omitempty" toml:"status"`                     // "modified"|"added"|"deleted"|"renamed"
	Binary          bool     `json:"binary,omitempty" toml:"binary"`                     // file must be a binary diff
	MinNetDeleted   int      `json:"minNetDeleted,omitempty" toml:"min_net_deleted"`     // del-add >= N in one file
	MinChangedLines int      `json:"minChangedLines,omitempty" toml:"min_changed_lines"` // add+del >= N in one file
	AddedPattern    string   `json:"addedPattern,omitempty" toml:"added_pattern"`        // RE2 over added lines' content
	MinTokenEntropy float64  `json:"minTokenEntropy,omitempty" toml:"min_token_entropy"` // bits/char threshold; requires MinTokenLen >= 8
	MinTokenLen     int      `json:"minTokenLen,omitempty" toml:"min_token_len"`

	// Worktree-wide conditions: a rule using any of these must not also set a
	// per-file condition above (Compile rejects the mix). All that are set on
	// one rule are ANDed, same as the per-file conditions.
	MinDeleteAddRatio float64 `json:"minDeleteAddRatio,omitempty" toml:"min_delete_add_ratio"` // del >= ratio*add
	MinFilesChanged   int     `json:"minFilesChanged,omitempty" toml:"min_files_changed"`      // len(files) >= N
	MinTotalChanged   int     `json:"minTotalChanged,omitempty" toml:"min_total_changed"`      // sum(add+del) >= N
}

func (r Rule) severityOrDefault() string {
	if r.Severity == "" {
		return "warn"
	}
	return r.Severity
}

// hasFileCond reports whether r sets at least one real per-file condition.
// ExcludeGlobs deliberately doesn't count: it's an exemption modifier, not a
// condition on its own — a rule with only exclude_globs set matches nothing.
func hasFileCond(r Rule) bool {
	return r.PathGlob != "" || len(r.PathGlobs) > 0 || r.Status != "" || r.Binary ||
		r.MinNetDeleted > 0 || r.MinChangedLines > 0 || r.AddedPattern != "" || r.MinTokenEntropy > 0
}

// hasWorktreeCond reports whether r sets at least one worktree-wide condition.
func hasWorktreeCond(r Rule) bool {
	return r.MinDeleteAddRatio > 0 || r.MinFilesChanged > 0 || r.MinTotalChanged > 0
}

// compiledRule pairs a validated Rule with its once-compiled added_pattern
// matcher(s) (empty unless AddedPattern is set). Rule itself stays pure data
// — the constitution bars code on a rule — so the compiled regexp lives only
// in this internal, Engine-private wrapper, never on Rule.
//
// A top-level alternation (A|B|C) is split into independently compiled
// branches at Compile time (see compileAddedPattern) — mathematically
// identical for a boolean "does this line match" check (L(A|B|C) =
// L(A)∪L(B)∪L(C) is a basic regular-language identity, unaffected by engine
// internals), but far cheaper in practice: Go's regexp can no longer share
// one fast literal-prefix scan across branches with different required
// prefixes, so it falls back to a slow backtracking search per call. Splitting
// lets each branch keep its own prefix optimisation; the shipped
// secrets-pattern rule alone drops from ~20ms to ~1ms over a 5k-line scan
// this way (BenchmarkEvalDefaultPackOn5kLineDiff). Never user-visible: the
// rule stays one named rule with one message either way.
type compiledRule struct {
	Rule
	addedRE []*regexp.Regexp
}

// addedPatternMatches reports whether s matches any of cr's added_pattern
// branches.
func (cr compiledRule) addedPatternMatches(s string) bool {
	for _, re := range cr.addedRE {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// compileAddedPattern compiles pattern (validating it as a side effect of the
// first Compile call), splitting a top-level alternation into independently
// compiled branches when possible — see compiledRule's doc comment — and
// falling back to the single combined pattern otherwise. Never changes
// matching behaviour, only its internal execution shape.
func compileAddedPattern(pattern string) ([]*regexp.Regexp, error) {
	combined, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	if split := splitAlternation(pattern); split != nil {
		return split, nil
	}
	return []*regexp.Regexp{combined}, nil
}

// splitAlternation returns pattern's top-level alternation branches,
// independently compiled, or nil if pattern doesn't parse to a top-level
// OpAlternate (or anything about re-compiling a branch in isolation fails) —
// in which case the caller keeps using the single combined regexp.
func splitAlternation(pattern string) []*regexp.Regexp {
	root, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil || root.Op != syntax.OpAlternate || len(root.Sub) < 2 {
		return nil
	}
	out := make([]*regexp.Regexp, 0, len(root.Sub))
	for _, sub := range root.Sub {
		re, err := regexp.Compile(sub.String())
		if err != nil {
			return nil // shouldn't happen; fail safe to the combined pattern
		}
		out = append(out, re)
	}
	return out
}

// Engine holds compiled, validated rules. Build with Compile.
type Engine struct {
	rules []compiledRule
}

// Compile validates rules and returns a ready-to-Eval Engine. It replaces the
// old can't-fail New: growing the rule language to include regex patterns and
// threshold combinations means a hand-edited (or agent-authored) rule pack can
// now be self-contradictory or fail to compile, and that must be caught at
// load time with a message naming the offending rule — the same "typo fails
// fast" philosophy config.Load already applies to the TOML shape itself.
//
// Validated, in order: severity enum; path_glob/path_globs mutual exclusion;
// per-file vs worktree-wide condition mixing (including exclude_globs on a
// worktree-wide rule); min_token_entropy requires min_token_len >= 8;
// added_pattern must compile as RE2; rule names must be unique once blank
// names are auto-filled ("rule-<1-based position>").
func Compile(rules []Rule) (*Engine, error) {
	named := make([]Rule, len(rules))
	copy(named, rules)
	for i := range named {
		if named[i].Name == "" {
			named[i].Name = fmt.Sprintf("rule-%d", i+1)
		}
	}

	seen := make(map[string]bool, len(named))
	compiled := make([]compiledRule, 0, len(named))
	for _, r := range named {
		if seen[r.Name] {
			return nil, fmt.Errorf("guardrail: duplicate rule name %q", r.Name)
		}
		seen[r.Name] = true

		switch r.Severity {
		case "", "warn", "danger":
		default:
			return nil, fmt.Errorf("guardrail: rule %q: invalid severity %q (want \"\", \"warn\", or \"danger\")", r.Name, r.Severity)
		}
		if r.PathGlob != "" && len(r.PathGlobs) > 0 {
			return nil, fmt.Errorf("guardrail: rule %q: path_glob and path_globs are mutually exclusive", r.Name)
		}
		fileCond, wtCond := hasFileCond(r), hasWorktreeCond(r)
		if fileCond && wtCond {
			return nil, fmt.Errorf("guardrail: rule %q: mixes a per-file condition with a worktree-wide condition", r.Name)
		}
		if len(r.ExcludeGlobs) > 0 && wtCond {
			return nil, fmt.Errorf("guardrail: rule %q: exclude_globs is not valid on a worktree-wide rule", r.Name)
		}
		if r.MinTokenEntropy > 0 && r.MinTokenLen < 8 {
			return nil, fmt.Errorf("guardrail: rule %q: min_token_entropy requires min_token_len >= 8 (got %d)", r.Name, r.MinTokenLen)
		}

		var addedRE []*regexp.Regexp
		if r.AddedPattern != "" {
			res, err := compileAddedPattern(r.AddedPattern)
			if err != nil {
				return nil, fmt.Errorf("guardrail: rule %q: invalid added_pattern: %w", r.Name, err)
			}
			addedRE = res
		}

		compiled = append(compiled, compiledRule{Rule: r, addedRE: addedRE})
	}
	return &Engine{rules: compiled}, nil
}

// Rules returns the configured rules (for display/debugging), in their final
// auto-named form.
func (e *Engine) Rules() []Rule {
	out := make([]Rule, len(e.rules))
	for i, cr := range e.rules {
		out[i] = cr.Rule
	}
	return out
}

// Eval returns every guardrail hit for the given diff. Per-file rules emit one
// hit per matching file (at most one, even when a content condition matches
// several lines — see scanContent); worktree-wide rules emit a single hit with
// an empty File.
func (e *Engine) Eval(d model.Diff) []model.GuardrailHit {
	var hits []model.GuardrailHit
	var totalAdd, totalDel int
	for _, f := range d.Files {
		totalAdd += f.Stats.Add
		totalDel += f.Stats.Del
	}
	filesChanged := len(d.Files)

	for _, cr := range e.rules {
		r := cr.Rule
		if hasWorktreeCond(r) {
			if !worktreeMatches(r, totalAdd, totalDel, filesChanged) {
				continue
			}
			hits = append(hits, model.GuardrailHit{
				Rule:     r.Name,
				Severity: r.severityOrDefault(),
				Message:  msgOr(r, defaultWorktreeMsg(r, totalAdd, totalDel, filesChanged)),
			})
			continue // ratio/files/total rules are worktree-wide only
		}

		if !hasFileCond(r) {
			continue
		}
		for _, f := range d.Files {
			matched, line, count := cr.fileMatches(f)
			if !matched {
				continue
			}
			hits = append(hits, model.GuardrailHit{
				Rule:     r.Name,
				Severity: r.severityOrDefault(),
				Message:  msgOr(r, defaultFileMsg(cr, f, count)),
				File:     f.Path,
				Line:     line,
			})
		}
	}
	return hits
}

// fileMatches reports whether f trips cr's per-file conditions (all ANDed),
// plus the first matching added line (content conditions only) and how many
// added lines matched — never the matched text itself (non-echo invariant).
func (cr compiledRule) fileMatches(f model.DiffFile) (matched bool, line, count int) {
	r := cr.Rule
	if r.PathGlob != "" && !GlobMatch(r.PathGlob, f.Path) {
		return false, 0, 0
	}
	if len(r.PathGlobs) > 0 && !anyGlobMatch(r.PathGlobs, f.Path) {
		return false, 0, 0
	}
	if len(r.ExcludeGlobs) > 0 && anyGlobMatch(r.ExcludeGlobs, f.Path) {
		return false, 0, 0
	}
	if r.Status != "" && string(f.Status) != r.Status {
		return false, 0, 0
	}
	if r.Binary && !f.Binary {
		return false, 0, 0
	}
	if r.MinNetDeleted > 0 && (f.Stats.Del-f.Stats.Add) < r.MinNetDeleted {
		return false, 0, 0
	}
	if r.MinChangedLines > 0 && (f.Stats.Add+f.Stats.Del) < r.MinChangedLines {
		return false, 0, 0
	}
	if len(cr.addedRE) > 0 || r.MinTokenEntropy > 0 {
		return scanContent(cr, f)
	}
	return true, 0, 0
}

// scanContent scans f's added lines (skipping binary files entirely, capped
// at maxAddedLinesScanned) for cr's content condition(s). When a rule sets
// both added_pattern and entropy, a line only counts when BOTH hold on it —
// the same AND-all-set-conditions rule as every other condition class.
// Returns whether the file trips, the first matching line's NewNum, and how
// many lines matched — count only, never the content: the non-echo invariant
// lives here at the source.
func scanContent(cr compiledRule, f model.DiffFile) (matched bool, line, count int) {
	if f.Binary {
		return false, 0, 0
	}
	scanned := 0
	for _, h := range f.Hunks {
		for _, ln := range h.Lines {
			if ln.Kind != model.LineAdd {
				continue
			}
			if scanned >= maxAddedLinesScanned {
				return matched, line, count
			}
			scanned++

			if len(cr.addedRE) > 0 && !cr.addedPatternMatches(ln.Content) {
				continue
			}
			if cr.MinTokenEntropy > 0 && !hasHighEntropyToken(ln.Content, cr.MinTokenLen, cr.MinTokenEntropy) {
				continue
			}
			if !matched {
				matched = true
				line = ln.NewNum
			}
			count++
		}
	}
	return matched, line, count
}

// worktreeMatches reports whether every worktree-wide condition r sets holds.
func worktreeMatches(r Rule, totalAdd, totalDel, filesChanged int) bool {
	if r.MinDeleteAddRatio > 0 {
		if !(totalDel > 0 && float64(totalDel) >= r.MinDeleteAddRatio*float64(max(totalAdd, 1)) && totalAdd < totalDel) {
			return false
		}
	}
	if r.MinFilesChanged > 0 && filesChanged < r.MinFilesChanged {
		return false
	}
	if r.MinTotalChanged > 0 && (totalAdd+totalDel) < r.MinTotalChanged {
		return false
	}
	return true
}

// defaultWorktreeMsg builds the fallback message for a worktree-wide hit when
// the rule sets no Message of its own, joining a clause per condition the
// rule actually set.
func defaultWorktreeMsg(r Rule, totalAdd, totalDel, filesChanged int) string {
	var parts []string
	if r.MinDeleteAddRatio > 0 {
		parts = append(parts, fmt.Sprintf("deletes %d lines vs %d added (%.1fx)", totalDel, totalAdd, ratio(totalDel, totalAdd)))
	}
	if r.MinFilesChanged > 0 {
		parts = append(parts, fmt.Sprintf("touches %d files", filesChanged))
	}
	if r.MinTotalChanged > 0 {
		parts = append(parts, fmt.Sprintf("total churn %d lines", totalAdd+totalDel))
	}
	return strings.Join(parts, "; ")
}

// defaultFileMsg builds the fallback message for a per-file hit when the rule
// sets no Message of its own. count is a match count only (content
// conditions) — never the matched text: the non-echo invariant.
func defaultFileMsg(cr compiledRule, f model.DiffFile, count int) string {
	r := cr.Rule
	var parts []string
	if r.MinNetDeleted > 0 {
		parts = append(parts, fmt.Sprintf("deletes %d lines (net %d)", f.Stats.Del, f.Stats.Del-f.Stats.Add))
	}
	if r.MinChangedLines > 0 {
		parts = append(parts, fmt.Sprintf("changes %d lines", f.Stats.Add+f.Stats.Del))
	}
	if len(cr.addedRE) > 0 {
		parts = append(parts, fmt.Sprintf("matches added_pattern (%d match%s)", count, plural(count)))
	}
	if r.MinTokenEntropy > 0 {
		parts = append(parts, fmt.Sprintf("high-entropy token in added content (%d match%s)", count, plural(count)))
	}
	if r.Binary {
		parts = append(parts, "binary file")
	}
	if r.Status != "" {
		parts = append(parts, fmt.Sprintf("status %s", r.Status))
	}
	if len(parts) > 0 {
		return strings.Join(parts, "; ")
	}
	switch {
	case r.PathGlob != "":
		return fmt.Sprintf("matches %s", r.PathGlob)
	case len(r.PathGlobs) > 0:
		return fmt.Sprintf("matches %s", strings.Join(r.PathGlobs, ", "))
	default:
		return "matches rule conditions"
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

func msgOr(r Rule, fallback string) string {
	if r.Message != "" {
		return r.Message
	}
	return fallback
}

func ratio(del, add int) float64 {
	if add == 0 {
		return float64(del)
	}
	return float64(del) / float64(add)
}

// anyGlobMatch reports whether path matches any of patterns.
func anyGlobMatch(patterns []string, path string) bool {
	for _, p := range patterns {
		if GlobMatch(p, path) {
			return true
		}
	}
	return false
}

// DefaultRules is the sensible starter set shipped when no config is present
// (P5-design.md §1.2). The v0.2 six survive under their original names (the
// "-root" + nested glob pairs collapse into one path_globs list each); the
// rest are new: secrets (pattern + entropy), lockfile/manifest churn,
// file-count/total-churn thresholds, a CI-workflow delete, and an added
// binary file.
func DefaultRules() []Rule {
	return []Rule{
		{Name: "touches-migrations", Severity: "danger", PathGlobs: []string{"migrations/**", "**/migrations/**"}, Message: "touches database migrations"},
		{Name: "edits-ci", Severity: "warn", PathGlobs: []string{".github/workflows/*", "**/.github/workflows/*"}, Message: "edits CI workflows"},
		{Name: "large-deletion", Severity: "warn", MinNetDeleted: 80, Message: "large net deletion in one file"},
		{Name: "net-negative", Severity: "warn", MinDeleteAddRatio: 3.0, Message: "deletes far more than it adds"},

		{
			Name:     "secrets-pattern",
			Severity: "danger",
			AddedPattern: strings.Join([]string{
				`AKIA[0-9A-Z]{16}`,
				`-----BEGIN [A-Z ]*PRIVATE KEY-----`,
				`ghp_[A-Za-z0-9]{36}`,
				`github_pat_[A-Za-z0-9_]{22,}`,
				`sk-[A-Za-z0-9_-]{20,}`,
				`xox[bpars]-[A-Za-z0-9-]{10,}`,
				`AIza[0-9A-Za-z_-]{35}`,
			}, "|"),
			Message: "secrets-shaped string (known token pattern)",
		},
		{
			Name:            "secrets-entropy",
			Severity:        "warn",
			MinTokenEntropy: 4.8,
			MinTokenLen:     32,
			ExcludeGlobs: []string{
				"go.sum", "**/go.sum",
				"*.lock", "**/*.lock",
				"package-lock.json", "**/package-lock.json",
				"pnpm-lock.yaml", "**/pnpm-lock.yaml",
				"*.snap", "**/*.snap",
				"*.svg", "**/*.svg",
				"*.min.js", "**/*.min.js",
			},
			Message: "high-entropy string — possible secret",
		},
		{
			Name:            "lockfile-churn",
			Severity:        "warn",
			PathGlobs:       []string{"go.sum", "**/go.sum", "*.lock", "**/*.lock", "package-lock.json", "**/package-lock.json", "pnpm-lock.yaml", "**/pnpm-lock.yaml", "*.snap", "**/*.snap"},
			MinChangedLines: 200,
			Message:         "large lockfile/snapshot churn",
		},
		{
			Name:      "deps-manifest-changed",
			Severity:  "warn",
			PathGlobs: []string{"go.mod", "**/go.mod", "package.json", "**/package.json", "Cargo.toml", "**/Cargo.toml"},
			Message:   "dependency manifest changed",
		},
		{Name: "big-blast-radius", Severity: "warn", MinFilesChanged: 25, Message: "touches 25+ files"},
		{Name: "huge-churn", Severity: "warn", MinTotalChanged: 1500, Message: "very large total churn"},
		{
			Name:      "ci-workflow-delete",
			Severity:  "danger",
			Status:    "deleted",
			PathGlobs: []string{".github/workflows/*", "**/.github/workflows/*"},
			Message:   "deletes a CI workflow",
		},
		{Name: "binary-added", Severity: "warn", Binary: true, Status: "added", Message: "adds a binary file"},
	}
}

// GlobMatch reports whether pattern matches path. Supports:
//   - "*"  matches any run of non-slash characters
//   - "**" matches any run of characters including slashes
//   - "?"  matches a single non-slash character
//
// It is anchored (must match the whole path).
func GlobMatch(pattern, path string) bool {
	return globHelper(pattern, path)
}

func globHelper(p, s string) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			if len(p) > 1 && p[1] == '*' {
				// "**" — consume it (and an optional trailing slash) and try to
				// match the remainder at every position, including across slashes.
				rest := p[2:]
				rest = strings.TrimPrefix(rest, "/")
				if rest == "" {
					return true // trailing ** matches anything
				}
				for i := 0; i <= len(s); i++ {
					if globHelper(rest, s[i:]) {
						return true
					}
				}
				return false
			}
			// single "*" — match zero+ non-slash chars.
			rest := p[1:]
			for i := 0; i <= len(s); i++ {
				if i > 0 && s[i-1] == '/' {
					break
				}
				if globHelper(rest, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 || s[0] == '/' {
				return false
			}
			p, s = p[1:], s[1:]
		default:
			if len(s) == 0 || s[0] != p[0] {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}
