package main

// Item A's daemon-level integration test: a real wtd subprocess over a temp
// root with two repos, one hidden by exclude_repos — /api/worktrees must
// omit it entirely (its worktrees are never enumerated, not just hidden),
// and /api/status must report both the active filter and the daemon's pid.
// wtdBin/requireWtdBin come from integration_test.go; waitForUnixSocket from
// notify_smoke_test.go; testGit from main_test.go (same package, per this
// repo's "test helpers aren't importable across packages" convention).

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// unixHTTPClient mirrors cmd/wt/main.go's own newClient dialer: an
// http.Client that always dials sockPath over "unix" regardless of the
// request URL's host — the only transport a real wtd subprocess speaks.
func unixHTTPClient(sockPath string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
	}
}

func getJSON(t *testing.T, hc *http.Client, url string, out any) {
	t.Helper()
	resp, err := hc.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("GET %s: decode: %v", url, err)
	}
}

func TestWtdDiscoveryFilterExcludesRepoAndStatusReportsFilterAndPid(t *testing.T) {
	bin := requireWtdBin(t)

	root := t.TempDir()
	keep := filepath.Join(root, "keep-me")
	drop := filepath.Join(root, "archive-old")
	for _, dir := range []string{keep, drop} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		testGit(t, dir, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		testGit(t, dir, "add", ".")
		testGit(t, dir, "commit", "-q", "-m", "init")
	}

	cfgPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("exclude_repos = [\"archive-*\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Short socket dir: sockaddr_un.sun_path is ~104 bytes on macOS, and
	// root (a t.TempDir(), which embeds the full test name) reliably blows
	// past that — see cmd/wt/integration_test.go's shortSocketDir.
	sockDir, err := os.MkdirTemp("", "wtd-filter-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "wtd.sock")
	statePath := filepath.Join(root, "state.json")

	cmd := exec.Command(bin,
		"-config", cfgPath,
		"-root", root,
		"-socket", sockPath,
		"-state", statePath,
		"-watch", "poll",
		"-interval", "150ms",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting wtd: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	waitForUnixSocket(t, sockPath, 5*time.Second)
	// Let the poller's immediate first scan (fired synchronously at startup)
	// complete before asserting on its result.
	time.Sleep(300 * time.Millisecond)

	hc := unixHTTPClient(sockPath)

	var wts []model.Worktree
	getJSON(t, hc, "http://unix/api/worktrees", &wts)
	found := false
	for _, w := range wts {
		if w.Repo == "archive-old" {
			t.Errorf("excluded repo archive-old appeared in /api/worktrees: %+v", w)
		}
		if w.Repo == "keep-me" {
			found = true
		}
	}
	if !found {
		t.Errorf("kept repo keep-me missing from /api/worktrees: %+v", wts)
	}

	var st statusPayload
	getJSON(t, hc, "http://unix/api/status", &st)
	if len(st.ExcludeRepos) != 1 || st.ExcludeRepos[0] != "archive-*" {
		t.Errorf("status ExcludeRepos = %v, want [archive-*]", st.ExcludeRepos)
	}
	if st.Pid != cmd.Process.Pid {
		t.Errorf("status Pid = %d, want %d (the wtd subprocess's own pid)", st.Pid, cmd.Process.Pid)
	}
}

// TestWtdIncludeExcludeOverlapExcludeWinsEndToEnd is the daemon-level sibling
// of internal/discovery's own TestDiscoverReposIncludeExcludeFilter "exclude
// wins over an overlapping include" case: that test proves the algorithm in
// isolation, but nothing before this proved that a real daemon actually
// threads BOTH include_repos and exclude_repos from config.Load through
// engine.Config into discovery.DiscoverRepos — the test above only exercises
// exclude_repos alone. archive-old matches both patterns here; exclude must
// still win, and keep-me (an include-only match) must remain.
func TestWtdIncludeExcludeOverlapExcludeWinsEndToEnd(t *testing.T) {
	bin := requireWtdBin(t)

	root := t.TempDir()
	keep := filepath.Join(root, "keep-me")
	drop := filepath.Join(root, "archive-old")
	for _, dir := range []string{keep, drop} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		testGit(t, dir, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		testGit(t, dir, "add", ".")
		testGit(t, dir, "commit", "-q", "-m", "init")
	}

	cfgPath := filepath.Join(root, "config.toml")
	cfgBody := "include_repos = [\"archive-*\", \"keep-me\"]\nexclude_repos = [\"archive-*\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}

	sockDir, err := os.MkdirTemp("", "wtd-overlap-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "wtd.sock")
	statePath := filepath.Join(root, "state.json")

	cmd := exec.Command(bin,
		"-config", cfgPath,
		"-root", root,
		"-socket", sockPath,
		"-state", statePath,
		"-watch", "poll",
		"-interval", "150ms",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting wtd: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	waitForUnixSocket(t, sockPath, 5*time.Second)
	time.Sleep(300 * time.Millisecond)

	hc := unixHTTPClient(sockPath)

	var wts []model.Worktree
	getJSON(t, hc, "http://unix/api/worktrees", &wts)
	found := false
	for _, w := range wts {
		if w.Repo == "archive-old" {
			t.Errorf("archive-old matches both include and exclude; exclude must win, but it appeared in /api/worktrees: %+v", w)
		}
		if w.Repo == "keep-me" {
			found = true
		}
	}
	if !found {
		t.Errorf("keep-me (include-only match) missing from /api/worktrees: %+v", wts)
	}

	var st statusPayload
	getJSON(t, hc, "http://unix/api/status", &st)
	if len(st.IncludeRepos) != 2 || len(st.ExcludeRepos) != 1 {
		t.Errorf("status IncludeRepos/ExcludeRepos = %v/%v, want both reported (2 include patterns, 1 exclude pattern)", st.IncludeRepos, st.ExcludeRepos)
	}
}

// stopWtdSubprocess sends SIGTERM to cmd and waits (bounded) for it to
// exit — used between the restarts in
// TestWtdReviewMarkDormantAcrossHideThenUnhideRestart below so the next
// daemon start never races the previous instance's own shutdown/socket
// cleanup (see cmd/wtd/signal_test.go for the same SIGTERM contract pinned
// in isolation).
func stopWtdSubprocess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signaling wtd: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("wtd did not exit within 5s of SIGTERM")
	}
}

// waitForWorktreeMatch polls GET /api/worktrees until some entry satisfies
// pred, returning it — used by the restart test below, where each phase
// needs to positively confirm a SPECIFIC worktree's state rather than just
// wait out a fixed sleep (as the simpler exclude-only/overlap tests above
// do).
func waitForWorktreeMatch(t *testing.T, hc *http.Client, timeout time.Duration, pred func(model.Worktree) bool) (model.Worktree, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []model.Worktree
	for time.Now().Before(deadline) {
		var wts []model.Worktree
		getJSON(t, hc, "http://unix/api/worktrees", &wts)
		last = wts
		for _, w := range wts {
			if pred(w) {
				return w, true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("no worktree matched within %s; last snapshot=%+v", timeout, last)
	return model.Worktree{}, false
}

// TestWtdReviewMarkDormantAcrossHideThenUnhideRestart is candidate gap #1
// from the discovery-filter test brief: a review mark made while a repo is
// visible must survive that repo being hidden by exclude_repos (the store
// keys review state by worktree id — a hash of the worktree's own path,
// independent of whatever the current discovery filter says) and reappear
// intact once the repo is unhidden again — dormant, never deleted. Three
// real wtd subprocesses share the same -state file across restarts, exactly
// like an operator toggling exclude_repos in their config.toml over time.
func TestWtdReviewMarkDormantAcrossHideThenUnhideRestart(t *testing.T) {
	bin := requireWtdBin(t)

	root := t.TempDir()
	keep := filepath.Join(root, "keep-me")
	hidden := filepath.Join(root, "archive-old")
	for _, dir := range []string{keep, hidden} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		testGit(t, dir, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		testGit(t, dir, "add", ".")
		testGit(t, dir, "commit", "-q", "-m", "init")
	}
	// A worktree with a real, uncommitted change in the repo that will later
	// be hidden — something to actually mark reviewed.
	featPath := filepath.Join(root, "archive-old-feature")
	testGit(t, hidden, "worktree", "add", "-q", "-b", "feature", featPath)
	if err := os.WriteFile(filepath.Join(featPath, "f.txt"), []byte("hi\nedited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sockDir, err := os.MkdirTemp("", "wtd-dormant-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "wtd.sock")
	statePath := filepath.Join(root, "state.json")

	// Explicit -config files for every phase (an empty one for "no filter"),
	// rather than relying on config.DefaultPath() being absent on whatever
	// machine runs this test — three sequential daemon restarts make this
	// test expensive to debug if an ambient ~/.config/wtcockpit/config.toml
	// ever interfered.
	emptyCfgPath := filepath.Join(root, "config-empty.toml")
	if err := os.WriteFile(emptyCfgPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	excludeCfgPath := filepath.Join(root, "config-exclude.toml")
	if err := os.WriteFile(excludeCfgPath, []byte("exclude_repos = [\"archive-*\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	baseArgs := []string{"-root", root, "-socket", sockPath, "-state", statePath, "-watch", "poll", "-interval", "100ms"}
	noFilterArgs := append([]string{"-config", emptyCfgPath}, baseArgs...)
	excludeArgs := append([]string{"-config", excludeCfgPath}, baseArgs...)

	// Phase 1: unfiltered — find the feature worktree and mark its one
	// changed file reviewed.
	cmd1 := exec.Command(bin, noFilterArgs...)
	if err := cmd1.Start(); err != nil {
		t.Fatalf("starting wtd (phase 1, unfiltered): %v", err)
	}
	waitForUnixSocket(t, sockPath, 5*time.Second)
	hc1 := unixHTTPClient(sockPath)

	feat, ok := waitForWorktreeMatch(t, hc1, 3*time.Second, func(w model.Worktree) bool {
		return w.Repo == "archive-old" && w.Branch == "feature"
	})
	if !ok {
		stopWtdSubprocess(t, cmd1)
		t.Fatal("archive-old's feature worktree never appeared before hiding it")
	}
	if feat.Stats.Files != 1 {
		stopWtdSubprocess(t, cmd1)
		t.Fatalf("feature worktree diff stats = %+v, want exactly 1 changed file", feat.Stats)
	}

	body, _ := json.Marshal(map[string]any{"id": feat.ID, "file": "f.txt", "reviewed": true})
	resp, err := hc1.Post("http://unix/api/review", "application/json", bytes.NewReader(body))
	if err != nil {
		stopWtdSubprocess(t, cmd1)
		t.Fatalf("POST /api/review: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		stopWtdSubprocess(t, cmd1)
		t.Fatalf("POST /api/review = %d, want 200", resp.StatusCode)
	}
	if _, ok := waitForWorktreeMatch(t, hc1, 3*time.Second, func(w model.Worktree) bool {
		return w.ID == feat.ID && w.Reviewed == 1
	}); !ok {
		stopWtdSubprocess(t, cmd1)
		t.Fatalf("review mark for %s never reflected in /api/worktrees", feat.ID)
	}
	stopWtdSubprocess(t, cmd1)

	// Phase 2: restart with exclude_repos=["archive-*"] active — the whole
	// repo, including the worktree just reviewed, must vanish from the API.
	cmd2 := exec.Command(bin, excludeArgs...)
	if err := cmd2.Start(); err != nil {
		t.Fatalf("starting wtd (phase 2, excluded): %v", err)
	}
	waitForUnixSocket(t, sockPath, 5*time.Second)
	hc2 := unixHTTPClient(sockPath)

	if _, ok := waitForWorktreeMatch(t, hc2, 3*time.Second, func(w model.Worktree) bool {
		return w.Repo == "keep-me"
	}); !ok {
		stopWtdSubprocess(t, cmd2)
		t.Fatal("keep-me never appeared after restarting with the exclude filter active")
	}
	var wts []model.Worktree
	getJSON(t, hc2, "http://unix/api/worktrees", &wts)
	for _, w := range wts {
		if w.Repo == "archive-old" {
			stopWtdSubprocess(t, cmd2)
			t.Fatalf("archive-old should be hidden by exclude_repos, but appeared: %+v", w)
		}
	}
	stopWtdSubprocess(t, cmd2)

	// Phase 3: restart unfiltered again — archive-old, and the phase-1 review
	// mark, must reappear untouched: dormant in the store, never deleted.
	cmd3 := exec.Command(bin, noFilterArgs...)
	if err := cmd3.Start(); err != nil {
		t.Fatalf("starting wtd (phase 3, unhidden again): %v", err)
	}
	waitForUnixSocket(t, sockPath, 5*time.Second)
	hc3 := unixHTTPClient(sockPath)

	final, ok := waitForWorktreeMatch(t, hc3, 3*time.Second, func(w model.Worktree) bool {
		return w.Repo == "archive-old" && w.Branch == "feature"
	})
	stopWtdSubprocess(t, cmd3)
	if !ok {
		t.Fatal("archive-old's feature worktree never reappeared after unhiding it")
	}
	if final.Reviewed != 1 {
		t.Errorf("review mark should have stayed dormant while archive-old was hidden, got Reviewed=%d after unhiding", final.Reviewed)
	}
}
