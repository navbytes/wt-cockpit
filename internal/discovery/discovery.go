// Package discovery scans root directories for git repositories. It identifies a
// repo by a `.git` *directory* (a main working tree); linked worktrees carry a
// `.git` *file* and are intentionally skipped here — the engine expands worktrees
// via the git backend, which dedupes them naturally. Discovery never descends into
// a repo or into heavy ignored directories, keeping the walk cheap even over a big
// ~/code tree.
package discovery

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// skipDirs are never descended into during discovery.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "target": true, "dist": true,
	"build": true, ".venv": true, "venv": true, "__pycache__": true,
	".next": true, ".cache": true, ".terraform": true,
}

// DiscoverRepos walks each root up to maxDepth levels and returns the repos found.
func DiscoverRepos(roots []string, maxDepth int) ([]model.Repo, error) {
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
			repos = append(repos, model.Repo{
				Name: filepath.Base(dir),
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
		if skipDirs[name] || (len(name) > 1 && name[0] == '.') {
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
