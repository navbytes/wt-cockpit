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
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// version is set at build time via -ldflags "-X main.version=...". "dev" is
// the fallback for a plain `go build`/`go run`.
var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"ls"}
	}

	// These run without ever touching the daemon — no protocol handshake
	// needed (or possible, if wtd isn't even running).
	switch args[0] {
	case "-version", "--version":
		printVersion(os.Stdout)
		return
	case "-h", "--help", "help":
		usage()
		return
	}

	home, _ := os.UserHomeDir()
	socket := filepath.Join(home, ".wtcockpit", "wtd.sock")
	if s := os.Getenv("WTD_SOCKET"); s != "" {
		socket = s
	}
	c := newClient(socket)

	// Every remaining command talks to wtd, so check the protocol handshake
	// first: one extra round trip per invocation, accepted (unix socket,
	// sub-ms) for the sake of failing loudly on a version mismatch instead of
	// silently misinterpreting a shape this build doesn't understand.
	if err := c.checkVersion(); err != nil {
		fatal("%v", err)
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
	case "status":
		must(c.status(len(args) > 1 && args[1] == "--json"))
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
	default:
		fatal("unknown command %q (try: ls, watch, diff, review, approve, refresh, status)", args[0])
	}
}

// printVersion writes the build-time version string to w. Pulled out of the
// -version flag branch so it's unit-testable without exercising os.Args/os.Exit.
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "wt %s (%s)\n", version, runtime.Version())
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

// checkVersion performs the daemon↔client protocol handshake: GET
// /api/version and confirm wtd speaks the same model.ProtocolVersion this
// client was built against. main runs it once before dispatching to any
// daemon-touching command.
func (c *client) checkVersion() error {
	resp, err := c.http.Get(c.base + "/api/version")
	if err != nil {
		return fmt.Errorf("cannot reach wtd (is it running?): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("wtd is v0.1 (no protocol handshake); rebuild/restart wtd")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("wtd returned %s", resp.Status)
	}

	var v struct {
		Protocol int `json:"protocol"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return fmt.Errorf("wtd /api/version: %w", err)
	}
	if v.Protocol != model.ProtocolVersion {
		return fmt.Errorf("protocol mismatch: wt speaks %d, wtd speaks %d — rebuild both from the same checkout and restart wtd", model.ProtocolVersion, v.Protocol)
	}
	return nil
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
	var event string
	for sc.Scan() {
		if shouldRenderSSELine(sc.Text(), &event) {
			render()
		}
	}
	return sc.Err()
}

// shouldRenderSSELine advances the SSE per-frame event-name state machine (an
// "event: <name>" line names the frame that follows; a blank line ends it)
// and reports whether the just-scanned line is a "data:" payload that should
// trigger a re-render. Every data line renders except one inside a "hello"
// frame — the version-handshake preamble handleEvents sends first (see
// cmd/wtd's handleEvents/currentVersion). wt already verified the handshake
// via GET /api/version before this command ran, so the hello frame is
// consumed here with no visible effect rather than causing a spurious render.
func shouldRenderSSELine(line string, event *string) bool {
	switch {
	case strings.HasPrefix(line, "event:"):
		*event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		return false
	case line == "":
		*event = ""
		return false
	case strings.HasPrefix(line, "data:"):
		return *event != "hello"
	default:
		return false
	}
}

func (c *client) diff(id string) error {
	var d model.Diff
	if err := c.get("/api/diff?id="+id, &d); err != nil {
		return err
	}
	renderDiff(d)
	return nil
}

// statusPayload mirrors /api/status's JSON shape. Kept local (like approve's
// anonymous result struct below) rather than importing the daemon's internal
// packages — wt only ever depends on the wire format, never on wtd's Go types.
type statusPayload struct {
	Version       string   `json:"version"`
	Protocol      int      `json:"protocol"`
	UptimeSeconds float64  `json:"uptimeSeconds"`
	SocketPath    string   `json:"socketPath"`
	WatcherMode   string   `json:"watcherMode"`
	Roots         []string `json:"roots"`
	StatePath     string   `json:"statePath"`
	RepoCount     int      `json:"repoCount"`
	WorktreeCount int      `json:"worktreeCount"`
	ReviewedFiles int      `json:"reviewedFiles"`
	TotalFiles    int      `json:"totalFiles"`
}

func (c *client) status(jsonOut bool) error {
	var st statusPayload
	if err := c.get("/api/status", &st); err != nil {
		return err
	}
	if jsonOut {
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	renderStatus(st)
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

func renderStatus(st statusPayload) {
	uptime := time.Duration(st.UptimeSeconds * float64(time.Second)).Round(time.Second)
	fmt.Printf("%s%swtd status%s\n", bold, blue, reset)
	fmt.Printf("  %sversion%s    %s (protocol %d)\n", dim, reset, st.Version, st.Protocol)
	fmt.Printf("  %suptime%s     %s\n", dim, reset, uptime)
	fmt.Printf("  %ssocket%s     %s\n", dim, reset, st.SocketPath)
	fmt.Printf("  %swatcher%s    %s\n", dim, reset, st.WatcherMode)
	fmt.Printf("  %sstate%s      %s\n", dim, reset, st.StatePath)
	fmt.Printf("  %sroots%s      %s\n", dim, reset, strings.Join(st.Roots, ", "))
	fmt.Printf("  %srepos%s      %d\n", dim, reset, st.RepoCount)
	fmt.Printf("  %sworktrees%s  %d\n", dim, reset, st.WorktreeCount)
	fmt.Printf("  %sreviewed%s   %d/%d files\n", dim, reset, st.ReviewedFiles, st.TotalFiles)
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
  wt status [--json]        daemon health: version, uptime, watcher, roots, review counts
  wt review <id> <file>     mark a file reviewed (--off to unmark)
  wt approve <id>           merge worktree→base & remove it (needs full review + clean tree)
  wt refresh                force a rescan
  wt -version               print the client's build version

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
