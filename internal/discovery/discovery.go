// Package discovery scans root directories for git repositories. It identifies a
// repo by a `.git` *directory* (a main working tree); linked worktrees carry a
// `.git` *file* and are intentionally skipped here — the engine expands worktrees
// via the git backend, which dedupes them naturally. Discovery never descends into
// a repo or into heavy ignored directories, keeping the walk cheap even over a big
// ~/code tree.
package discovery

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// SkipDirs are never descended into during discovery. Exported so other
// filesystem walkers (notably the fsnotify watcher) apply the exact same
// skip-list instead of duplicating it.
var SkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "target": true, "dist": true,
	"build": true, ".venv": true, "venv": true, "__pycache__": true,
	".next": true, ".cache": true, ".terraform": true,
}

// DiscoverRepos walks each root up to maxDepth levels and returns the repos
// found, filtered by include/exclude glob lists matched against each repo's
// folder BASENAME (config.Config.IncludeRepos/ExcludeRepos): include empty
// admits everything; include non-empty requires at least one match; exclude
// always wins regardless of include. A filtered-out repo is dropped here, so
// it is never enumerated further by any caller (its worktrees never reach
// the engine or the fsnotify watcher) — both nil is the "no filter" case
// every existing caller/test predates this feature with.
func DiscoverRepos(roots []string, maxDepth int, include, exclude []string) ([]model.Repo, error) {
	seen := map[string]bool{}
	var repos []model.Repo

	for _, root := range roots {
		absRoot, err := filepath.Abs(expandHome(root))
		if err != nil {
			continue
		}
		err = walk(absRoot, absRoot, maxDepth, func(dir string) {
			if seen[dir] {
				return
			}
			seen[dir] = true
			name := filepath.Base(dir)
			if !repoPasses(name, include, exclude) {
				slog.Debug("discovery: repo skipped by include/exclude filter", "repo", name, "path", dir)
				return
			}
			repos = append(repos, model.Repo{
				Name: name,
				Path: dir,
				Lang: detectLang(dir),
			})
		})
		if err != nil {
			return repos, err
		}
	}
	return repos, nil
}

// repoPasses reports whether a repo's folder basename survives the
// include-then-exclude filter (config.toml's include_repos/exclude_repos):
// an empty include list admits everything at that stage; a non-empty one
// requires at least one match. Exclude is evaluated after, and always wins —
// a basename matching any exclude pattern is dropped even if it also matched
// an include pattern.
func repoPasses(name string, include, exclude []string) bool {
	if len(include) > 0 && !anyGlobMatch(include, name, false) {
		return false
	}
	return !anyGlobMatch(exclude, name, true)
}

// anyGlobMatch reports whether name matches any of patterns, using stdlib
// path/filepath.Match only (never a custom matcher — see config.Load's
// checkGlobSyntax, which rejects every syntactically invalid pattern at
// config-load time, before it ever reaches here). onErrorMatch is a
// backstop for filepath.Match still returning an error at scan time despite
// that upfront check (only reachable if stdlib's own Match grammar ever
// drifts from what checkGlobSyntax mirrors) — it decides which way THIS
// caller must fail:
//   - include (onErrorMatch=false): an errored pattern never itself admits
//     a repo, so a broken include pattern can only ever narrow admission,
//     the same direction Match's own error return already implies.
//   - exclude (onErrorMatch=true): an errored pattern is treated as if it
//     HAD matched, so a broken exclude pattern hides the repo rather than
//     silently leaving it visible — failing OPEN is the dangerous direction
//     for exclude specifically, since its whole job is to hide repos.
//
// ponytail: no dedup/rate-limit on the slog.Warn below — checkGlobSyntax
// means this path is essentially unreachable in practice, so a real hit
// warning once per repo per scan (rather than once per process) is an
// acceptable cost for staying loud about an otherwise-silent miscount; add
// throttling if a genuine stdlib grammar drift ever makes this noisy.
func anyGlobMatch(patterns []string, name string, onErrorMatch bool) bool {
	for _, pat := range patterns {
		ok, err := filepath.Match(pat, name)
		if err != nil {
			slog.Warn("discovery: glob pattern errored at match time; failing closed",
				"pattern", pat, "repo", name, "treatedAsMatch", onErrorMatch)
			if onErrorMatch {
				return true
			}
			continue
		}
		if ok {
			return true
		}
	}
	return false
}

// walk descends dir, calling onRepo for any directory holding a `.git` dir and not
// descending into it. Heavy/ignored dirs and dot-dirs are skipped.
func walk(dir, root string, maxDepth int, onRepo func(string)) error {
	rel, _ := filepath.Rel(root, dir)
	if depth(rel) > maxDepth {
		return nil
	}

	// Is this dir a main repo? (.git present and a directory)
	if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
		onRepo(dir)
		return nil // do not descend into a repo
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // unreadable dir: skip, don't fail the whole scan
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if SkipDirs[name] || (len(name) > 1 && name[0] == '.') {
			continue
		}
		child := filepath.Join(dir, name)
		// Resolve symlinked dirs carefully: os.ReadDir already returns type; avoid
		// following symlinks to prevent cycles.
		if isSymlink(e) {
			continue
		}
		if err := walk(child, root, maxDepth, onRepo); err != nil {
			return err
		}
	}
	return nil
}

func isSymlink(e fs.DirEntry) bool {
	return e.Type()&fs.ModeSymlink != 0
}

func depth(rel string) int {
	if rel == "." || rel == "" {
		return 0
	}
	d := 1
	for _, r := range rel {
		if r == filepath.Separator {
			d++
		}
	}
	return d
}

// detectLang makes a best-effort language tag from marker files.
func detectLang(dir string) string {
	markers := []struct {
		file string
		lang string
	}{
		{"go.mod", "Go"},
		{"Cargo.toml", "Rust"},
		{"package.json", "TS/JS"},
		{"tsconfig.json", "TS/JS"},
		{"pyproject.toml", "Python"},
		{"requirements.txt", "Python"},
		{"Gemfile", "Ruby"},
		{"pom.xml", "Java"},
		{"build.gradle", "Java"},
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(dir, m.file)); err == nil {
			return m.lang
		}
	}
	// Terraform: any *.tf file.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".tf" {
				return "Terraform"
			}
		}
	}
	return ""
}

func expandHome(p string) string {
	if len(p) >= 2 && p[0] == '~' && (p[1] == '/' || p == "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}
