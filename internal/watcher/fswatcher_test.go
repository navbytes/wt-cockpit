package watcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
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

// TestFSWatcherCommitInLinkedWorktreeTriggersRefresh is MN3's fixed version:
// the previous form wrote an *untracked* new.go before committing, so
// rec.waitFor was trivially satisfiable by that working-tree Create event
// alone — the test would still pass even if the gitdir (HEAD/index/logs)
// watch were deleted entirely. To actually pin gitdir-driven detection, this
// drains the one working-tree event a content edit necessarily produces
// *before* committing, then commits with no further working-tree write at
// all (`git commit -am`, not `git add` + a new file write) and requires a
// FRESH onChange(featurePath) after that point — something only the gitdir
// watch (index/HEAD/logs, all internal to .git) can produce.
func TestFSWatcherCommitInLinkedWorktreeTriggersRefresh(t *testing.T) {
	root, _, featurePath := buildRepoWithLinkedWorktree(t)
	rec := newRecorder()
	cancel, done := runWatcher(t, newTestFSWatcher(root), rec)
	defer func() { cancel(); <-done }()

	os.WriteFile(filepath.Join(featurePath, "app.go"), []byte("package app\n\nfunc B() {}\n"), 0o644)
	rec.waitFor(t, featurePath, 3*time.Second)

	before := len(rec.snapshot())
	git(t, featurePath, "commit", "-q", "-am", "feature work")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(rec.snapshot()) <= before {
		time.Sleep(20 * time.Millisecond)
	}
	if len(rec.snapshot()) <= before {
		t.Fatalf("commit alone (gitdir HEAD/index/logs, no further working-tree write) should trigger a fresh onChange(%q); calls=%v", featurePath, rec.snapshot())
	}
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

// TestReconcileDropsBookkeepingForRemovedWorktree is a whitebox unit test of
// fsSession.reconcile: once a worktree disappears from `git worktree list`,
// the next reconcile must drop its watched-dir and dirCount bookkeeping —
// otherwise a stale root would linger forever, permanently eating into the
// per-worktree fd cap and (if a path were ever reused) able to misroute a
// future event to a dead root.
func TestReconcileDropsBookkeepingForRemovedWorktree(t *testing.T) {
	root, repoPath, featurePath := buildRepoWithLinkedWorktree(t)
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer fsw.Close()

	sess := &fsSession{
		fsw:       fsw,
		be:        gitbackend.NewCLI(),
		watched:   map[string]watchEntry{},
		dirCount:  map[string]int{},
		capLogged: map[string]bool{},
	}
	sess.reconcile([]string{root}, 4, nil, nil)

	if _, ok := sess.dirCount[featurePath]; !ok {
		t.Fatalf("precondition: feature worktree should be tracked after the first reconcile, got %+v", sess.dirCount)
	}

	git(t, repoPath, "worktree", "remove", "--force", featurePath)
	sess.reconcile([]string{root}, 4, nil, nil)

	if _, ok := sess.dirCount[featurePath]; ok {
		t.Errorf("dirCount should drop the removed worktree, got %+v", sess.dirCount)
	}
	for path, entry := range sess.watched {
		if entry.root == featurePath {
			t.Errorf("watched map still has an entry for the removed worktree: %s -> %+v", path, entry)
		}
	}
}

// TestFSWatcherWorktreeRemoveDoesNotPanic runs `git worktree remove` on a
// worktree while it is actively watched via the real Run() event loop (not
// the whitebox reconcile call above). A panic in the event-handling goroutine
// would crash the whole test binary, so simply completing is most of the
// assertion; the rest confirms the session is still healthy afterwards (not
// wedged) by proving a different, still-existing worktree keeps triggering.
func TestFSWatcherWorktreeRemoveDoesNotPanic(t *testing.T) {
	root, repoPath, featurePath := buildRepoWithLinkedWorktree(t)
	rec := newRecorder()
	cancel, done := runWatcher(t, newTestFSWatcher(root), rec)
	defer func() { cancel(); <-done }()

	before := len(rec.snapshot())
	git(t, repoPath, "worktree", "remove", "--force", featurePath)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(rec.snapshot()) <= before {
		time.Sleep(20 * time.Millisecond)
	}
	if len(rec.snapshot()) <= before {
		t.Fatalf("expected the watcher to signal a change after worktree removal, got none; calls=%v", rec.snapshot())
	}

	// Still alive & correctly tracking afterwards: editing the still-existing
	// main worktree keeps triggering its own onChange.
	os.WriteFile(filepath.Join(repoPath, "app.go"), []byte("package app\n\nfunc Q() {}\n"), 0o644)
	rec.waitFor(t, repoPath, 3*time.Second)
}

// TestFSWatcherRapidMkdirThenWriteEventuallySurfaces exercises the
// watch-escalation race named in the T2 handoff: a subdirectory is created
// and immediately written into, with no delay for the Create event to be
// processed and a watch installed on it before the write happens (contrast
// with TestFSWatcherEscalatesIntoNewSubdirectory, which deliberately waits
// for escalation to land first). Even if escalation loses that race, the
// periodic reconciliation tick's unconditional onChange("") — fired every
// tick regardless of whether anything was detected — must still surface the
// change within one interval. A short interval is used here (unlike the
// package default 30s) specifically so this safety net is what the test can
// observe within a normal timeout.
func TestFSWatcherRapidMkdirThenWriteEventuallySurfaces(t *testing.T) {
	root, _, featurePath := buildRepoWithLinkedWorktree(t)
	rec := newRecorder()
	w := &FSWatcher{Roots: []string{root}, Interval: 500 * time.Millisecond, Backend: gitbackend.NewCLI()}
	cancel, done := runWatcher(t, w, rec)
	defer func() { cancel(); <-done }()

	before := len(rec.snapshot())
	newDir := filepath.Join(featurePath, "rapidpkg")
	// No settling sleep before the write, on purpose: mkdir and the write race
	// the watcher's own subdirectory-watch escalation.
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(newDir, "new.go"), []byte("package rapidpkg\n"), 0o644)

	// Generous eventual assertion: either escalation won the race (featurePath
	// reported directly) or the reconciliation tick's unconditional
	// onChange("") papered over a lost Create event — either outcome means
	// the change wasn't lost forever, which is the documented guarantee.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range rec.snapshot()[before:] {
			if c == featurePath || c == "" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("neither escalation nor the reconciliation tick surfaced the rapid mkdir+write; calls=%v", rec.snapshot())
}

// TestFSWatcherContextCancelDuringEventStormReturnsCleanly cancels ctx while
// a goroutine is actively hammering the watched worktree with writes, unlike
// TestFSWatcherContextCancelStopsCleanly (which cancels with nothing in
// flight). This targets shutdown races between the debounce timers' AfterFunc
// goroutines (each trying to send on `fired`) and Run's own goroutine
// returning — exactly what -race is for.
func TestFSWatcherContextCancelDuringEventStormReturnsCleanly(t *testing.T) {
	root, _, featurePath := buildRepoWithLinkedWorktree(t)
	rec := newRecorder()
	cancel, done := runWatcher(t, newTestFSWatcher(root), rec)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		target := filepath.Join(featurePath, "app.go")
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				os.WriteFile(target, []byte("package app\n\n// "+string(rune('a'+i%26))+"\n"), 0o644)
				i++
				time.Sleep(time.Millisecond)
			}
		}
	}()

	time.Sleep(50 * time.Millisecond) // let the storm actually get going
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s of ctx cancel during an event storm")
	}
	close(stop)
	wg.Wait()
}

// TestFSWatcherTracksTwoReposUnderOneRoot: a single FSWatcher over one root
// containing two independent (sibling, non-nested) repos must track each
// one's edits individually — proving discovery + per-worktree watching both
// work across multiple repos, not just multiple worktrees of one repo.
func TestFSWatcherTracksTwoReposUnderOneRoot(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	repoA := filepath.Join(root, "repo-a")
	repoB := filepath.Join(root, "repo-b")
	for _, r := range []string{repoA, repoB} {
		if err := os.MkdirAll(r, 0o755); err != nil {
			t.Fatal(err)
		}
		git(t, r, "init", "-q", "-b", "main")
		os.WriteFile(filepath.Join(r, "app.go"), []byte("package app\n"), 0o644)
		git(t, r, "add", ".")
		git(t, r, "commit", "-q", "-m", "init")
	}

	rec := newRecorder()
	cancel, done := runWatcher(t, newTestFSWatcher(root), rec)
	defer func() { cancel(); <-done }()

	os.WriteFile(filepath.Join(repoA, "app.go"), []byte("package app\n\nfunc A() {}\n"), 0o644)
	rec.waitFor(t, repoA, 3*time.Second)

	os.WriteFile(filepath.Join(repoB, "app.go"), []byte("package app\n\nfunc B() {}\n"), 0o644)
	rec.waitFor(t, repoB, 3*time.Second)
}
