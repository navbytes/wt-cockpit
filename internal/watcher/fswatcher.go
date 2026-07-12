package watcher

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/navbytes/wt-cockpit/internal/discovery"
	"github.com/navbytes/wt-cockpit/internal/gitbackend"
)

// maxWatchedDirsPerWorktree bounds how many directories FSWatcher will watch
// inside a single worktree's working tree. fsnotify's kqueue backend (macOS/BSD)
// opens a file descriptor per watched entry, so this is really an fd budget.
//
// ponytail: this caps watched DIRECTORIES; kqueue also opens an fd per FILE
// inside a watched dir, so real fd usage runs higher than the dir count alone.
// Past the cap we stop escalating and lean on the reconciliation tick — revisit
// with a file-count-aware budget if fd exhaustion shows up in practice.
const maxWatchedDirsPerWorktree = 4096

// gitStateNames are the files inside a worktree's gitdir whose change is
// authoritative for "something happened": HEAD/index move via atomic rename
// (so they must never be watched directly — only their containing directory),
// MERGE_HEAD appears/disappears across a merge, packed-refs moves on `git
// pack-refs`. Everything else in the gitdir (locks, COMMIT_EDITMSG, ORIG_HEAD,
// ...) is noise we deliberately ignore.
var gitStateNames = map[string]bool{
	"HEAD": true, "index": true, "MERGE_HEAD": true, "packed-refs": true,
}

// globalKey is the debounce key for events that should trigger a full
// reconcile + rescan rather than a single worktree's refresh (a repo's
// .git/worktrees dir gaining or losing an entry).
const globalKey = "\x00global"

// watchKind records why a directory is being watched, so events from it can be
// filtered and routed correctly.
type watchKind int

const (
	kindWork              watchKind = iota // inside a worktree's working tree
	kindGitDir                             // a worktree's resolved gitdir (HEAD, index, ...)
	kindGitLogs                            // a worktree's gitdir/logs (reflogs; logs/HEAD matters)
	kindReposWorktreesDir                  // a repo's .git/worktrees (worktree add/remove)
)

type watchEntry struct {
	root string // owning worktree's working-tree path (unused for kindReposWorktreesDir)
	kind watchKind
}

// FSWatcher is a git-state-first fsnotify watcher: it watches each worktree's
// gitdir (never the files inside it — git replaces HEAD/index via atomic
// rename, which breaks a file-level watch) plus its working tree, and debounces
// bursts of events into a single targeted refresh hint per worktree. A
// reconciliation tick provides a slower-but-certain fallback: it re-runs
// discovery so new repos/worktrees are picked up, and requests a full rescan in
// case any fs event was missed (unreliable network mounts, dropped kqueue/
// inotify events, etc).
//
// FSWatcher does its own repo/worktree discovery (via Backend) because it must
// set up watches before it can usefully wait for events — unlike Poller, which
// simply hands the whole job to the engine's own Refresh.
type FSWatcher struct {
	Roots    []string
	MaxDepth int                // discovery depth; <=0 defaults to 4 (mirrors engine.Config)
	Interval time.Duration      // reconciliation tick; <=0 defaults to 10s
	Backend  gitbackend.Backend // used to list each repo's worktrees; must be non-nil
}

// Run watches every worktree under Roots and invokes onChange(worktreeRoot) on
// a debounced (~150ms trailing edge) change, or onChange("") on the initial
// scan and every reconciliation tick. It blocks until ctx is cancelled, at
// which point it tears down the fsnotify watcher and returns.
func (w *FSWatcher) Run(ctx context.Context, onChange func(path string)) {
	interval := w.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	maxDepth := w.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 4
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("fswatcher: fsnotify unavailable; falling back to polling", "error", err)
		(&Poller{Interval: interval}).Run(ctx, onChange)
		return
	}
	defer fsw.Close()

	sess := &fsSession{
		fsw:       fsw,
		be:        w.Backend,
		watched:   map[string]watchEntry{},
		dirCount:  map[string]int{},
		capLogged: map[string]bool{},
	}
	sess.reconcile(w.Roots, maxDepth)
	onChange("") // initial scan, mirrors Poller's immediate first tick

	// Trailing-edge debounce: an event for a key (a worktree root, or globalKey)
	// (re)starts a 150ms timer; onChange fires only once the key goes quiet.
	// Firing happens on the AfterFunc's own goroutine via `fired`, but it always
	// hands off to this loop rather than calling onChange itself, so onChange is
	// invoked from a single goroutine only — same as Poller.
	const debounce = 150 * time.Millisecond
	pending := map[string]*time.Timer{}
	fired := make(chan string)
	trigger := func(key string) {
		if t, ok := pending[key]; ok {
			t.Reset(debounce)
			return
		}
		pending[key] = time.AfterFunc(debounce, func() {
			select {
			case fired <- key:
			case <-ctx.Done():
			}
		})
	}
	defer func() {
		for _, t := range pending {
			t.Stop()
		}
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-fsw.Events:
			if !ok {
				return
			}
			sess.handle(ev, trigger)
		case err, ok := <-fsw.Errors:
			if !ok {
				return
			}
			slog.Warn("fswatcher error", "error", err)
		case key := <-fired:
			if key == globalKey {
				sess.reconcile(w.Roots, maxDepth)
				onChange("")
			} else {
				onChange(key)
			}
		case <-ticker.C:
			sess.reconcile(w.Roots, maxDepth)
			onChange("")
		}
	}
}

// fsSession holds the mutable watch state for one Run invocation. It is only
// ever touched from Run's single goroutine, so it needs no locking.
type fsSession struct {
	fsw       *fsnotify.Watcher
	be        gitbackend.Backend
	watched   map[string]watchEntry // watched directory -> what it means
	dirCount  map[string]int        // worktree root -> watched dir count (the cap)
	capLogged map[string]bool       // worktree root -> cap warning already logged
}

// reconcile re-runs discovery, adds watches for any new repo or worktree, and
// drops bookkeeping for worktrees that disappeared (fsnotify itself already
// auto-removes the underlying watch when a path is deleted).
func (s *fsSession) reconcile(roots []string, maxDepth int) {
	repos, err := discovery.DiscoverRepos(roots, maxDepth)
	if err != nil {
		return
	}

	known := map[string]bool{}
	for _, repo := range repos {
		s.watchReposWorktreesDir(repo.Path)
		refs, err := s.be.ListWorktrees(repo.Path)
		if err != nil {
			continue // a bad repo shouldn't sink the whole reconcile
		}
		for _, ref := range refs {
			known[ref.Path] = true
			if _, already := s.dirCount[ref.Path]; already {
				continue
			}
			s.addWorktree(ref.Path)
		}
	}

	for path, entry := range s.watched {
		if entry.kind != kindReposWorktreesDir && !known[entry.root] {
			delete(s.watched, path)
		}
	}
	for root := range s.dirCount {
		if !known[root] {
			delete(s.dirCount, root)
			delete(s.capLogged, root)
		}
	}
}

// watchReposWorktreesDir watches a repo's .git/worktrees directory so a linked
// worktree being added or removed is noticed without waiting for the
// reconciliation tick. The directory doesn't exist until the first `git
// worktree add`; reconcile retries it on every tick, so it's picked up as soon
// as it appears.
func (s *fsSession) watchReposWorktreesDir(repoPath string) {
	dir := filepath.Join(repoPath, ".git", "worktrees")
	if _, ok := s.watched[dir]; ok {
		return
	}
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		if err := s.fsw.Add(dir); err == nil {
			s.watched[dir] = watchEntry{kind: kindReposWorktreesDir}
		}
	}
}

// addWorktree sets up the gitdir + working-tree watches for a newly-seen
// worktree root.
func (s *fsSession) addWorktree(root string) {
	s.dirCount[root] = 0
	if gitDir, err := resolveGitDir(root); err == nil {
		if err := s.fsw.Add(gitDir); err == nil {
			s.watched[gitDir] = watchEntry{root: root, kind: kindGitDir}
		}
		logs := filepath.Join(gitDir, "logs")
		if err := s.fsw.Add(logs); err == nil {
			s.watched[logs] = watchEntry{root: root, kind: kindGitLogs}
		}
	}
	s.addTree(root, root)
}

// addTree recursively watches dir and its subdirectories (up to the per-
// worktree cap), skipping ".git" and the discovery skip-list. It's used both
// for a worktree's initial working-tree walk (dir == root) and for escalating
// into a directory created after Run started (dir == the new subdirectory).
//
// ponytail: no per-file gitignore parsing — skip-list + reconciliation tick;
// revisit if noise shows up.
func (s *fsSession) addTree(root, dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil // best-effort: an unreadable entry shouldn't abort the walk
		}
		if path != dir {
			name := d.Name()
			if name == ".git" || discovery.SkipDirs[name] {
				return filepath.SkipDir
			}
		}
		if s.dirCount[root] >= maxWatchedDirsPerWorktree {
			if !s.capLogged[root] {
				slog.Warn("fswatcher watched-dir cap hit; relying on reconciliation tick", "root", root, "cap", maxWatchedDirsPerWorktree)
				s.capLogged[root] = true
			}
			return filepath.SkipDir
		}
		if err := s.fsw.Add(path); err != nil {
			return nil
		}
		s.watched[path] = watchEntry{root: root, kind: kindWork}
		s.dirCount[root]++
		return nil
	})
}

// handle routes one fsnotify event to the owning worktree (or the global key),
// filtering gitdir noise and escalating watches into newly-created
// directories, then debounces a trigger for whoever owns it.
func (s *fsSession) handle(ev fsnotify.Event, trigger func(key string)) {
	dir := filepath.Dir(ev.Name)
	owner, ok := s.watched[dir]
	if !ok {
		return // not something we're watching for (e.g. a sibling we don't track)
	}
	name := filepath.Base(ev.Name)

	switch owner.kind {
	case kindReposWorktreesDir:
		trigger(globalKey)
		return
	case kindGitDir:
		if !gitStateNames[name] {
			return
		}
	case kindGitLogs:
		if name != "HEAD" {
			return
		}
	case kindWork:
		if ev.Has(fsnotify.Create) && name != ".git" && !discovery.SkipDirs[name] {
			if info, err := os.Lstat(ev.Name); err == nil && info.IsDir() {
				s.addTree(owner.root, ev.Name)
			}
		}
	}

	// A watched directory itself disappearing/moving: fsnotify auto-removes the
	// underlying watch, so drop our bookkeeping too (keeps the cap recoverable
	// instead of a one-way ratchet).
	if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
		if _, tracked := s.watched[ev.Name]; tracked {
			delete(s.watched, ev.Name)
			if owner.kind == kindWork {
				s.dirCount[owner.root]--
			}
		}
	}

	trigger(owner.root)
}

// resolveGitDir returns the directory containing HEAD/index/logs for the
// worktree at path: the ".git" directory itself for a main worktree, or the
// resolved target of a linked worktree's ".git" FILE (which reads
// "gitdir: <path>", pointing into <main>/.git/worktrees/<name>).
func resolveGitDir(worktreePath string) (string, error) {
	p := filepath.Join(worktreePath, ".git")
	info, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return p, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(b))
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("watcher: unexpected .git file contents in %s", worktreePath)
	}
	dir := strings.TrimPrefix(line, prefix)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(worktreePath, dir)
	}
	return filepath.Clean(dir), nil
}
