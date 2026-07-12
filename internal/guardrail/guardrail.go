// Package guardrail evaluates declarative rules over a worktree diff and returns
// the tripped rules. Rules are data (globs + thresholds), never code, so growing
// from 2 rules to 20 is a config change. Evaluation is pure and allocation-light.
package guardrail

import (
	"fmt"
	"strings"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// Rule is one declarative guardrail. A rule may combine conditions; a file (or the
// whole worktree, for ratio rules) trips the rule when its conditions all hold.
type Rule struct {
	Name     string `json:"name" toml:"name"`
	Severity string `json:"severity" toml:"severity"` // "warn" | "danger"
	Message  string `json:"message" toml:"message"`

	// Per-file conditions:
	PathGlob      string `json:"pathGlob,omitempty" toml:"path_glob"`            // e.g. "migrations/**"
	MinNetDeleted int    `json:"minNetDeleted,omitempty" toml:"min_net_deleted"` // del-add >= N in one file

	// Worktree-wide conditions:
	MinDeleteAddRatio float64 `json:"minDeleteAddRatio,omitempty" toml:"min_delete_add_ratio"`
}

func (r Rule) severityOrDefault() string {
	if r.Severity == "" {
		return "warn"
	}
	return r.Severity
}

// Engine holds compiled rules.
type Engine struct {
	rules []Rule
}

// New builds an Engine from rules.
func New(rules []Rule) *Engine { return &Engine{rules: rules} }

// Rules returns the configured rules (for display/debugging).
func (e *Engine) Rules() []Rule { return e.rules }

// Eval returns every guardrail hit for the given diff. Per-file rules emit one hit
// per matching file; worktree-wide rules emit a single hit with an empty File.
func (e *Engine) Eval(d model.Diff) []model.GuardrailHit {
	var hits []model.GuardrailHit
	var totalAdd, totalDel int
	for _, f := range d.Files {
		totalAdd += f.Stats.Add
		totalDel += f.Stats.Del
	}

	for _, r := range e.rules {
		// Worktree-wide: delete/add ratio.
		if r.MinDeleteAddRatio > 0 {
			if totalDel > 0 && float64(totalDel) >= r.MinDeleteAddRatio*float64(max(totalAdd, 1)) && totalAdd < totalDel {
				hits = append(hits, model.GuardrailHit{
					Rule:     r.Name,
					Severity: r.severityOrDefault(),
					Message:  msgOr(r, fmt.Sprintf("deletes %d lines vs %d added (%.1fx)", totalDel, totalAdd, ratio(totalDel, totalAdd))),
				})
			}
			continue // ratio rules are worktree-wide only
		}

		// Per-file rules.
		for _, f := range d.Files {
			if !e.fileMatches(r, f) {
				continue
			}
			hits = append(hits, model.GuardrailHit{
				Rule:     r.Name,
				Severity: r.severityOrDefault(),
				Message:  msgOr(r, defaultFileMsg(r, f)),
				File:     f.Path,
			})
		}
	}
	return hits
}

func (e *Engine) fileMatches(r Rule, f model.DiffFile) bool {
	// A per-file rule with no per-file condition matches nothing.
	hasCond := r.PathGlob != "" || r.MinNetDeleted > 0
	if !hasCond {
		return false
	}
	if r.PathGlob != "" && !GlobMatch(r.PathGlob, f.Path) {
		return false
	}
	if r.MinNetDeleted > 0 && (f.Stats.Del-f.Stats.Add) < r.MinNetDeleted {
		return false
	}
	return true
}

func defaultFileMsg(r Rule, f model.DiffFile) string {
	if r.MinNetDeleted > 0 {
		return fmt.Sprintf("deletes %d lines (net %d)", f.Stats.Del, f.Stats.Del-f.Stats.Add)
	}
	return fmt.Sprintf("matches %s", r.PathGlob)
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

// DefaultRules is a sensible starter set shipped when no config is present.
func DefaultRules() []Rule {
	return []Rule{
		{Name: "touches-migrations", Severity: "danger", PathGlob: "**/migrations/**", Message: "touches database migrations"},
		{Name: "touches-migrations-root", Severity: "danger", PathGlob: "migrations/**", Message: "touches database migrations"},
		{Name: "edits-ci", Severity: "warn", PathGlob: "**/.github/workflows/*", Message: "edits CI workflows"},
		{Name: "edits-ci-root", Severity: "warn", PathGlob: ".github/workflows/*", Message: "edits CI workflows"},
		{Name: "large-deletion", Severity: "warn", MinNetDeleted: 80, Message: "large net deletion in one file"},
		{Name: "net-negative", Severity: "warn", MinDeleteAddRatio: 3.0, Message: "deletes far more than it adds"},
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
