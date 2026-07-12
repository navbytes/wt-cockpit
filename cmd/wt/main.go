// Command wt is the terminal client for the cockpit. It is a thin client: it holds
// no git logic and only talks to wtd over the Unix socket. The `ls`/`watch` views
// render the radar; `diff` renders a worktree's diff; `review` toggles files.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

func main() {
	home, _ := os.UserHomeDir()
	socket := filepath.Join(home, ".wtcockpit", "wtd.sock")
	if s := os.Getenv("WTD_SOCKET"); s != "" {
		socket = s
	}
	c := newClient(socket)

	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"ls"}
	}
	switch args[0] {
	case "ls":
		must(c.ls())
	case "watch":
		must(c.watch())
	case "diff":
		if len(args) < 2 {
			fatal("usage: wt diff <id>")
		}
		must(c.diff(args[1]))
	case "review":
		if len(args) < 3 {
			fatal("usage: wt review <id> <file> [--off]")
		}
		reviewed := true
		if len(args) > 3 && args[3] == "--off" {
			reviewed = false
		}
		must(c.review(args[1], args[2], reviewed))
	case "approve":
		if len(args) < 2 {
			fatal("usage: wt approve <id>")
		}
		must(c.approve(args[1]))
	case "refresh":
		must(c.post("/api/refresh", nil, nil))
		fmt.Println("refreshed")
	case "-h", "--help", "help":
		usage()
	default:
		fatal("unknown command %q (try: ls, watch, diff, review, refresh)", args[0])
	}
}

// ---- client ----

type client struct {
	http *http.Client
	base string
}

func newClient(socket string) *client {
	return &client{
		base: "http://unix",
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

func (c *client) get(path string, out any) error {
	resp, err := c.http.Get(c.base + path)
	if err != nil {
		return fmt.Errorf("cannot reach wtd (is it running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("wtd returned %s", resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *client) post(path string, in, out any) error {
	var body *strings.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = strings.NewReader(string(b))
	} else {
		body = strings.NewReader("")
	}
	resp, err := c.http.Post(c.base+path, "application/json", body)
	if err != nil {
		return fmt.Errorf("cannot reach wtd (is it running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		// The daemon puts the human-readable reason (e.g. a refused gate) in the body.
		msg, _ := io.ReadAll(resp.Body)
		if s := strings.TrimSpace(string(msg)); s != "" {
			return fmt.Errorf("%s", s)
		}
		return fmt.Errorf("wtd returned %s", resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *client) ls() error {
	var wts []model.Worktree
	if err := c.get("/api/worktrees", &wts); err != nil {
		return err
	}
	renderRadar(wts)
	return nil
}

func (c *client) watch() error {
	// Re-render on every event. Simple and correct; the fancy TUI would do partial
	// updates, but this proves the live stream end to end.
	render := func() {
		var wts []model.Worktree
		if err := c.get("/api/worktrees", &wts); err == nil {
			fmt.Print("\033[2J\033[H") // clear + home
			renderRadar(wts)
			fmt.Printf("\n%swatching — ctrl-c to exit%s\n", dim, reset)
		}
	}
	render()
	resp, err := c.http.Get(c.base + "/api/events")
	if err != nil {
		return fmt.Errorf("cannot reach wtd: %w", err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			render()
		}
	}
	return sc.Err()
}

func (c *client) diff(id string) error {
	var d model.Diff
	if err := c.get("/api/diff?id="+id, &d); err != nil {
		return err
	}
	renderDiff(d)
	return nil
}

func (c *client) review(id, file string, reviewed bool) error {
	err := c.post("/api/review", map[string]any{"id": id, "file": file, "reviewed": reviewed}, nil)
	if err != nil {
		return err
	}
	state := "reviewed"
	if !reviewed {
		state = "un-reviewed"
	}
	fmt.Printf("marked %s %s in %s\n", file, state, id)
	return nil
}

func (c *client) approve(id string) error {
	var res struct {
		Merged  string `json:"merged"`
		Into    string `json:"into"`
		Removed string `json:"removed"`
	}
	if err := c.post("/api/approve", map[string]any{"id": id}, &res); err != nil {
		return err
	}
	fmt.Printf("%s✓ merged %s → %s%s and removed the worktree\n", green, res.Merged, res.Into, reset)
	return nil
}

// ---- rendering ----

const (
	reset  = "\033[0m"
	dim    = "\033[38;5;244m"
	faint  = "\033[38;5;240m"
	white  = "\033[97m"
	green  = "\033[38;5;71m"
	red    = "\033[38;5;203m"
	amber  = "\033[38;5;179m"
	blue   = "\033[38;5;75m"
	purple = "\033[38;5;177m"
	bold   = "\033[1m"
)

func agentColor(a model.AgentKind) string {
	switch a {
	case model.AgentClaude:
		return purple
	case model.AgentCodex:
		return green
	case model.AgentAider:
		return blue
	default:
		return dim
	}
}

func stateDot(s model.WorktreeState) string {
	switch s {
	case model.StateActive:
		return green + "●" + reset
	case model.StateDirty:
		return amber + "●" + reset
	default:
		return faint + "○" + reset
	}
}

func renderRadar(wts []model.Worktree) {
	// Group by repo for a tree-like layout, but keep global most-recent-first order
	// by iterating the already-sorted slice and printing repo headers on change.
	var active, alerts int
	for _, w := range wts {
		if w.State == model.StateActive {
			active++
		}
		alerts += len(w.Guardrails)
	}
	fmt.Printf("%s%swt cockpit%s  %s%d worktrees · %d active · %d guardrail hits%s\n\n",
		bold, blue, reset, dim, len(wts), active, alerts, reset)

	// Stable repo grouping: collect repos in first-seen order.
	byRepo := map[string][]model.Worktree{}
	var order []string
	for _, w := range wts {
		if _, ok := byRepo[w.Repo]; !ok {
			order = append(order, w.Repo)
		}
		byRepo[w.Repo] = append(byRepo[w.Repo], w)
	}
	sort.Strings(order)

	for _, repo := range order {
		fmt.Printf("%s%s%s\n", faint, repo, reset)
		for _, w := range byRepo[repo] {
			alert := ""
			if len(w.Guardrails) > 0 {
				sev := amber
				for _, h := range w.Guardrails {
					if h.Severity == "danger" {
						sev = red
					}
				}
				alert = "  " + sev + "⚠ " + shortGuard(w.Guardrails) + reset
			}
			fmt.Printf("  %s %s%-24s%s %s%-12s%s  %s+%-4d%s %s-%-4d%s %s%d files%s  %s%s%s%s\n",
				stateDot(w.State),
				white, truncate(w.Name, 24), reset,
				agentColor(w.Agent), w.Agent, reset,
				green, w.Stats.Add, reset,
				red, w.Stats.Del, reset,
				dim, w.Stats.Files, reset,
				faint, w.ID, reset,
				alert,
			)
		}
	}
	fmt.Printf("\n%s↳ wt diff <id>   ·   wt review <id> <file>   ·   wt watch%s\n", dim, reset)
}

func shortGuard(hits []model.GuardrailHit) string {
	seen := map[string]bool{}
	var parts []string
	for _, h := range hits {
		if !seen[h.Rule] {
			seen[h.Rule] = true
			parts = append(parts, h.Rule)
		}
	}
	if len(parts) > 2 {
		parts = append(parts[:2], "…")
	}
	return strings.Join(parts, ", ")
}

func renderDiff(d model.Diff) {
	fmt.Printf("%s%sdiff%s  base %s%s%s  %s%d files%s\n\n", bold, blue, reset, blue, d.Base, reset, dim, len(d.Files), reset)
	for _, f := range d.Files {
		tag := string(f.Status)
		fmt.Printf("%s%s%s %s(%s, +%d -%d)%s\n", bold, f.Path, reset, dim, tag, f.Stats.Add, f.Stats.Del, reset)
		if f.Binary {
			fmt.Printf("  %sbinary file%s\n\n", faint, reset)
			continue
		}
		for _, h := range f.Hunks {
			fmt.Printf("%s%s%s\n", blue, h.Header, reset)
			for _, ln := range h.Lines {
				switch ln.Kind {
				case model.LineAdd:
					fmt.Printf("%s+%s%s\n", green, ln.Content, reset)
				case model.LineDel:
					fmt.Printf("%s-%s%s\n", red, ln.Content, reset)
				default:
					fmt.Printf("%s %s%s\n", dim, ln.Content, reset)
				}
			}
		}
		fmt.Println()
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ---- helpers ----

func usage() {
	fmt.Print(`wt — worktree cockpit client

  wt ls                     list worktrees (radar)
  wt watch                  live radar, updates on every change
  wt diff <id>              show a worktree's diff
  wt review <id> <file>     mark a file reviewed (--off to unmark)
  wt approve <id>           merge worktree→base & remove it (needs full review + clean tree)
  wt refresh                force a rescan

Set WTD_SOCKET to override the daemon socket path.
`)
}

func must(err error) {
	if err != nil {
		fatal("%v", err)
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "wt: "+format+"\n", a...)
	os.Exit(1)
}
