package watcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/gitbackend"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// buildRepoWithLinkedWorktree makes a <root>/repo main worktree (one commit)
// plus a linked "feature" worktree, mirroring engine_test.go's buildWorkspace —
// a real repo on disk, never a mocked git backend.
func buildRepoWithLinkedWorktree(t *testing.T) (root, repoPath, featurePath string) {
	t.Helper()
	root = t.TempDir()
	// Resolve symlinks up front: on macOS t.TempDir() lives under /var, a
	// symlink to /private/var, and `git worktree list --porcelain` reports the
	// resolved (canonical) path. Watcher and engine both derive worktree paths
	// from that same git output, so this keeps the test's expectations aligned
	// with what onChange actually receives.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	repoPath = filepath.Join(root, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repoPath, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repoPath, "app.go"), []byte("package app\n"), 0o644)
	git(t, repoPath, "add", ".")
	git(t, repoPath, "commit", "-q", "-m", "init")

	featurePath = filepath.Join(root, "repo-feature")
	git(t, repoPath, "worktree", "add", "-q", "-b", "feature", featurePath)
	return root, repoPath, featurePath
}

// recorder collects onChange calls and lets a test wait for a given path to
// show up, or for the watcher to signal it has completed its initial scan.
type recorder struct {
	mu    sync.Mutex
	calls []string
	ready chan struct{}
	once  sync.Once
}

func newRecorder() *recorder {
	return &recorder{ready: make(chan struct{})}
}

func (r *recorder) onChange(path string) {
	r.mu.Lock()
	r.calls = append(r.calls, path)
	r.mu.Unlock()
	r.once.Do(func() { close(r.ready) })
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// waitFor polls until want is among the recorded calls, or fails after timeout.
func (r *recorder) waitFor(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, c := range r.snapshot() {
			if c == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for onChange(%q); got %v", timeout, want, r.snapshot())
}

// waitReady blocks until the watcher's initial scan (its first onChange call)
// has happened, so tests never race the goroutine that's still installing
// watches.
func (r *recorder) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-r.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher never became ready (no initial onChange)")
	}
}

// newTestFSWatcher uses a long reconciliation interval so tests only pass
// because fsnotify events (and escalation) actually work — not because the
// slow-path tick papered over a bug.
func newTestFSWatcher(root string) *FSWatcher {
	return &FSWatcher{
		Roots:    []string{root},
		Interval: 30 * time.Second,
		Backend:  gitbackend.NewCLI(),
	}
}

func runWatcher(t *testing.T, w *FSWatcher, rec *recorder) (cancel func(), done <-chan struct{}) {
	t.Helper()
	ctx, cancelFn := context.WithCancel(context.Background())
	d := make(chan struct{})
	go func() {
		w.Run(ctx, rec.onChange)
		close(d)
	}()
	rec.waitReady(t)
	return cancelFn, d
}

func TestFSWatcherFileEditTriggersTargetedRefresh(t *testing.T) {
	root, _, featurePath := buildRepoWithLinkedWorktree(t)
	rec := newRecorder()
	cancel, done := runWatcher(t, newTestFSWatcher(root), rec)
	defer func() { cancel(); <-done }()

	os.WriteFile(filepath.Join(featurePath, "app.go"), []byte("package app\n\nfunc B() {}\n"), 0o644)

	rec.waitFor(t, featurePath, 3*time.Second)
}

func TestFSWatcherCommitInLinkedWorktreeTriggersRefresh(t *testing.T) {
	root, _, featurePath := buildRepoWithLinkedWorktree(t)
	rec := newRecorder()
	cancel, done := runWatcher(t, newTestFSWatcher(root), rec)
	defer func() { cancel(); <-done }()

	os.WriteFile(filepath.Join(featurePath, "new.go"), []byte("package app\n"), 0o644)
	git(t, featurePath, "add", "-A")
	git(t, featurePath, "commit", "-q", "-m", "feature work")

	// The commit alone (HEAD/index/logs in the linked worktree's gitdir) must
	// surface a change, independent of whatever the earlier file write did.
	rec.waitFor(t, featurePath, 3*time.Second)
}

func TestFSWatcherCoalescesBurstOfWrites(t *testing.T) {
	root, _, featurePath := buildRepoWithLinkedWorktree(t)
	rec := newRecorder()
	cancel, done := runWatcher(t, newTestFSWatcher(root), rec)
	defer func() { cancel(); <-done }()

	target := filepath.Join(featurePath, "app.go")
	for i := 0; i < 20; i++ {
		os.WriteFile(target, []byte("package app\n\n// "+string(rune('a'+i))+"\n"), 0o644)
	}

	rec.waitFor(t, featurePath, 3*time.Second)
	// Let anything in flight settle well past the 150ms debounce window before
	// counting — a loose bound, not an exact count (per-platform event batching
	// can still cause more than one).
	time.Sleep(500 * time.Millisecond)

	n := 0
	for _, c := range rec.snapshot() {
		if c == featurePath {
			n++
		}
	}
	if n >= 5 {
		t.Errorf("20 rapid writes produced %d onChange(%q) calls, want a small coalesced number (<5)", n, featurePath)
	}
}

func TestFSWatcherEscalatesIntoNewSubdirectory(t *testing.T) {
	root, _, featurePath := buildRepoWithLinkedWorktree(t)
	rec := newRecorder()
	cancel, done := runWatcher(t, newTestFSWatcher(root), rec)
	defer func() { cancel(); <-done }()

	newDir := filepath.Join(featurePath, "newpkg")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Give the watcher goroutine a moment to see the Create event and install a
	// watch on the new directory before we write into it — otherwise the write
	// could land in the (real, if brief) window before escalation completes.
	time.Sleep(300 * time.Millisecond)
	os.WriteFile(filepath.Join(newDir, "new.go"), []byte("package newpkg\n"), 0o644)

	rec.waitFor(t, featurePath, 3*time.Second)
}

func TestFSWatcherContextCancelStopsCleanly(t *testing.T) {
	root, _, _ := buildRepoWithLinkedWorktree(t)
	rec := newRecorder()
	cancel, done := runWatcher(t, newTestFSWatcher(root), rec)

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s of ctx cancel")
	}
}
