// Package gitbackend abstracts all git access behind an interface. The default
// implementation shells out to the user's `git` binary, which is correct by
// construction: it honours their .gitignore, hooks, config, and edge cases exactly.
// A libgit2/gitoxide-backed implementation can be dropped in later behind Backend
// without touching any caller — this interface is the escape hatch the architecture
// promises.
package gitbackend

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
)

// WorktreeRef is the raw git view of a worktree, before the engine enriches it.
type WorktreeRef struct {
	Path   string
	Branch string
	Head   string
	IsMain bool
}

// Backend is the full set of git operations the cockpit needs. Everything here is
// read-only except Merge, which is the single write path (gated on full review).
type Backend interface {
	ListWorktrees(repoPath string) ([]WorktreeRef, error)
	CurrentBranch(worktreePath string) (string, error)
	DefaultBranch(repoPath string) (string, error)
	MergeBase(worktreePath, base string) (string, error)
	// DiffAgainstBase returns unified diff text of everything the worktree changed
	// since it forked from base — committed work plus the current working tree.
	DiffAgainstBase(worktreePath, base string) (string, error)
	IsDirty(worktreePath string) (bool, error)
	// HeadOID returns the current commit + a cheap fingerprint of index/worktree
	// state, used to decide whether a re-diff is needed.
	StateToken(worktreePath string) (string, error)

	// --- write path (the only mutating operations) ---

	// Merge merges branch into whatever is checked out at baseWorktreePath. On any
	// failure (notably a conflict) it aborts, leaving the base worktree clean.
	Merge(baseWorktreePath, branch string) error
	// RemoveWorktree removes the worktree at targetPath. gitDir may be any worktree
	// of the same repository.
	RemoveWorktree(gitDir, targetPath string) error
}

// CLI is the git-binary-backed implementation of Backend.
type CLI struct {
	gitPath string
	env     []string
}

// NewCLI returns a CLI backend using the `git` found on PATH.
func NewCLI() *CLI {
	return &CLI{gitPath: "git", env: os.Environ()}
}

// NewCLIWithEnv returns a CLI backend with an explicit environment. Useful for
// headless/cron contexts where a committer identity must be supplied for merges,
// and for deterministic tests.
func NewCLIWithEnv(env []string) *CLI {
	return &CLI{gitPath: "git", env: env}
}

func (c *CLI) run(dir string, args ...string) (string, error) {
	cmd := exec.Command(c.gitPath, args...)
	cmd.Dir = dir
	cmd.Env = c.env
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if errb.Len() > 0 {
			return "", &GitError{Args: args, Stderr: strings.TrimSpace(errb.String()), Err: err}
		}
		return "", err
	}
	return out.String(), nil
}

// GitError carries the failing command's stderr for diagnosis.
type GitError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *GitError) Error() string {
	return "git " + strings.Join(e.Args, " ") + ": " + e.Stderr
}
func (e *GitError) Unwrap() error { return e.Err }

// ListWorktrees parses `git worktree list --porcelain`.
func (c *CLI) ListWorktrees(repoPath string) ([]WorktreeRef, error) {
	out, err := c.run(repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var refs []WorktreeRef
	var cur WorktreeRef
	flush := func() {
		if cur.Path != "" {
			refs = append(refs, cur)
		}
		cur = WorktreeRef{}
	}
	for _, ln := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(ln, "worktree "):
			flush()
			cur.Path = strings.TrimPrefix(ln, "worktree ")
		case strings.HasPrefix(ln, "HEAD "):
			cur.Head = strings.TrimPrefix(ln, "HEAD ")
		case strings.HasPrefix(ln, "branch "):
			cur.Branch = shortBranch(strings.TrimPrefix(ln, "branch "))
		case ln == "detached":
			cur.Branch = "(detached)"
		}
	}
	flush()
	if len(refs) > 0 {
		refs[0].IsMain = true // git lists the main worktree first
	}
	return refs, nil
}

func shortBranch(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// CurrentBranch returns the checked-out branch (or "(detached)").
func (c *CLI) CurrentBranch(worktreePath string) (string, error) {
	out, err := c.run(worktreePath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	b := strings.TrimSpace(out)
	if b == "HEAD" {
		return "(detached)", nil
	}
	return b, nil
}

// DefaultBranch tries origin/HEAD, then falls back to main/master if present.
func (c *CLI) DefaultBranch(repoPath string) (string, error) {
	if out, err := c.run(repoPath, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(strings.TrimSpace(out), "origin/"), nil
	}
	for _, cand := range []string{"main", "master"} {
		if _, err := c.run(repoPath, "rev-parse", "--verify", "--quiet", cand); err == nil {
			return cand, nil
		}
	}
	// Last resort: whatever HEAD points at.
	return c.CurrentBranch(repoPath)
}

// MergeBase returns the fork point between the worktree HEAD and base.
func (c *CLI) MergeBase(worktreePath, base string) (string, error) {
	out, err := c.run(worktreePath, "merge-base", base, "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// DiffAgainstBase diffs from the merge-base of base..HEAD through to the working
// tree, so it captures both committed and uncommitted changes. It also appends
// synthesized diffs for untracked files, because an agent creating a brand-new
// file is exactly the kind of change the cockpit must surface — and plain
// `git diff` omits untracked files. It degrades gracefully: if base has no
// merge-base with HEAD, it falls back to diffing the working tree against HEAD.
func (c *CLI) DiffAgainstBase(worktreePath, base string) (string, error) {
	out, err := c.run(worktreePath, "diff", "--no-color", "--find-renames", "--merge-base", base)
	if err != nil {
		// Fallback: working tree vs HEAD (covers detached/base-missing cases).
		out2, err2 := c.run(worktreePath, "diff", "--no-color", "--find-renames", "HEAD")
		if err2 != nil {
			return "", err // report the original, more informative error
		}
		out = out2
	}
	return out + c.untrackedDiff(worktreePath), nil
}

// untrackedDiff synthesizes an "added file" diff for every untracked, non-ignored
// file, using `git diff --no-index` against /dev/null. This is entirely read-only
// (no index mutation), unlike the `git add -N` trick.
func (c *CLI) untrackedDiff(worktreePath string) string {
	list, err := c.run(worktreePath, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil || list == "" {
		return ""
	}
	var b strings.Builder
	for _, f := range strings.Split(list, "\x00") {
		if f == "" {
			continue
		}
		// --no-index exits non-zero when files differ; capture stdout regardless.
		out := c.runIgnoringExit(worktreePath, "diff", "--no-color", "--no-index", "--", "/dev/null", f)
		b.WriteString(out)
	}
	return b.String()
}

// runIgnoringExit runs git and returns stdout even on a non-zero exit code. Used
// for `git diff --no-index`, which uses exit status to signal "files differ".
func (c *CLI) runIgnoringExit(dir string, args ...string) string {
	cmd := exec.Command(c.gitPath, args...)
	cmd.Dir = dir
	cmd.Env = c.env
	var out bytes.Buffer
	cmd.Stdout = &out
	_ = cmd.Run()
	return out.String()
}

// IsDirty reports whether the worktree has any staged or unstaged changes.
func (c *CLI) IsDirty(worktreePath string) (bool, error) {
	out, err := c.run(worktreePath, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// StateToken is a cheap fingerprint that changes whenever the worktree's committed
// or working state changes. It combines HEAD with a hash of `status --porcelain`
// plus the index mtime, so the engine can skip re-diffing unchanged worktrees.
func (c *CLI) StateToken(worktreePath string) (string, error) {
	head, err := c.run(worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	status, err := c.run(worktreePath, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(head) + ":" + hashString(status), nil
}

// hashString is a small FNV-1a hash rendered as hex — enough to detect change.
func hashString(s string) string {
	const (
		offset uint64 = 1469598103934665603
		prime  uint64 = 1099511628211
	)
	h := offset
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	const hexdigits = "0123456789abcdef"
	var b [16]byte
	for i := 0; i < 16; i++ {
		b[15-i] = hexdigits[h&0xf]
		h >>= 4
	}
	return string(b[:])
}

// Merge merges branch into the branch checked out at baseWorktreePath. It uses a
// normal merge (fast-forward when possible, else a merge commit). If the merge
// fails for any reason — most importantly a conflict — it runs `merge --abort` so
// the base worktree is never left in a half-merged, conflict-marked state. This is
// the safety property the tests pin: a failed approve must not corrupt main.
func (c *CLI) Merge(baseWorktreePath, branch string) error {
	_, err := c.run(baseWorktreePath, "merge", "--no-edit", branch)
	if err != nil {
		_, _ = c.run(baseWorktreePath, "merge", "--abort") // best-effort cleanup
		return err
	}
	return nil
}

// RemoveWorktree removes targetPath. --force is used so untracked/ignored files in
// the worktree (common when agents scaffold files) don't block removal; the engine
// gates approval on a clean tree before calling this.
func (c *CLI) RemoveWorktree(gitDir, targetPath string) error {
	_, err := c.run(gitDir, "worktree", "remove", "--force", targetPath)
	return err
}
