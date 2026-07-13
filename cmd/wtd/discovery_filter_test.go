package main

// Item A's daemon-level integration test: a real wtd subprocess over a temp
// root with two repos, one hidden by exclude_repos — /api/worktrees must
// omit it entirely (its worktrees are never enumerated, not just hidden),
// and /api/status must report both the active filter and the daemon's pid.
// wtdBin/requireWtdBin come from integration_test.go; waitForUnixSocket from
// notify_smoke_test.go; testGit from main_test.go (same package, per this
// repo's "test helpers aren't importable across packages" convention).

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
