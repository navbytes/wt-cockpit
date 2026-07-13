// Command wt is the terminal client for the cockpit. It is a thin client: it holds
// no git logic and only talks to wtd over the Unix socket. The `ls`/`watch` views
// render the radar; `diff` renders a worktree's diff; `review` toggles files.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/navbytes/wt-cockpit/internal/buildinfo"
	wtclient "github.com/navbytes/wt-cockpit/internal/client"
	"github.com/navbytes/wt-cockpit/internal/diffparse"
	"github.com/navbytes/wt-cockpit/internal/guardrail"
	"github.com/navbytes/wt-cockpit/internal/model"
	"github.com/navbytes/wt-cockpit/internal/notify"
	"github.com/navbytes/wt-cockpit/internal/tui"
)

// version is set at build time via -ldflags "-X main.version=...". "dev" is
// the fallback for a plain `go build`/`go run`.
var version = "dev"

// isTerminal reports whether stdout is a TTY — the seam bare `wt`'s default
// routes on (P3-design.md §2.1). A var, not a bare call inline, so tests can
// fake it without needing a real terminal.
var isTerminal = func() bool { return isatty.IsTerminal(os.Stdout.Fd()) }

// defaultCommand is what bare `wt` (no args) runs. A TTY opens the full TUI —
// the "daily driver" goal a default-on-TTY flip is meant to deliver; anything
// else (a pipe, a redirect, cron, a script) keeps the existing `ls` text
// output so `wt | grep` and scripts never break. `wt ls`/`wt tui` are
// unaffected: both stay explicit, TTY-independent entries.
func defaultCommand(isTTY bool) string {
	if isTTY {
		return "tui"
	}
	return "ls"
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{defaultCommand(isTerminal())}
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

	socket := wtclient.SocketFromEnv()

	// wt tui is its own program, not a one-shot request: unlike every other
	// command, a down daemon is a retryable "connecting…" state, not an
	// instant fatal error, so it does its own handshake inside Init rather
	// than going through the blocking pre-flight check below.
	if args[0] == "tui" {
		if err := tui.Run(socket); err != nil {
			fatal("%v", err)
		}
		return
	}

	// wt menubar is a plugin-host emitter (SwiftBar/xbar, P5-design.md §1.7),
	// not an interactive invocation: xbar re-execs it on a schedule and
	// treats a nonzero exit or empty output as a broken plugin, so a down
	// daemon must render as a degraded plugin state ("wt ◦"), never fatal()'s
	// exit(1) — the one command that skips the shared checkVersion-or-fatal
	// preflight below entirely, exactly like tui's own early return above.
	if args[0] == "menubar" {
		renderMenubar(os.Stdout, newClient(socket))
		return
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
		must(c.cl.Refresh(context.Background()))
		fmt.Println("refreshed")
	case "comments":
		if len(args) < 2 {
			fatal("usage: wt comments <id> [--json] [--all]")
		}
		jsonOut, all := false, false
		for _, a := range args[2:] {
			switch a {
			case "--json":
				jsonOut = true
			case "--all":
				all = true
			default:
				fatal("usage: wt comments <id> [--json] [--all]")
			}
		}
		must(c.comments(args[1], jsonOut, all))
	case "comment":
		if len(args) < 4 {
			fatal("usage: wt comment <id> <file> <line> [--old] [--author <name>] <body...>")
		}
		line, err := strconv.Atoi(args[3])
		if err != nil {
			fatal("invalid line %q: must be an integer (0 = file-level)", args[3])
		}
		side, author, body := parseCommentArgs(args[4:])
		if body == "" {
			fatal("usage: wt comment <id> <file> <line> [--old] [--author <name>] <body...>")
		}
		must(c.comment(args[1], args[2], line, side, author, body))
	case "resolve":
		if len(args) < 3 {
			fatal("usage: wt resolve <id> <comment-id>")
		}
		must(c.resolve(args[1], args[2]))
	case "open":
		if len(args) < 2 {
			fatal("usage: wt open <id>")
		}
		must(c.open(args[1]))
	case "rules":
		if len(args) < 2 {
			fatal("usage: wt rules <id> [--json]")
		}
		must(c.rules(args[1], len(args) > 2 && args[2] == "--json"))
	default:
		fatal("unknown command %q (try: ls, watch, diff, review, approve, refresh, comments, comment, resolve, open, rules, menubar, status, tui)", args[0])
	}
}

// parseCommentArgs scans the trailing tokens of `wt comment` for its
// optional [--old] [--author <name>] flags, then joins whatever remains into
// the comment body (spaces preserved between words). Flags may appear in any
// order but must precede the body — the first token that isn't a recognised
// flag, and everything after it, is the body verbatim.
func parseCommentArgs(rest []string) (side, author, body string) {
	i := 0
	for i < len(rest) {
		switch rest[i] {
		case "--old":
			side = "old"
			i++
		case "--author":
			if i+1 < len(rest) {
				author = rest[i+1]
			}
			i += 2
		default:
			return side, author, strings.Join(rest[i:], " ")
		}
	}
	return side, author, ""
}

// printVersion writes the build-time version string to w. Pulled out of the
// -version flag branch so it's unit-testable without exercising os.Args/os.Exit.
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "wt %s (%s)\n", buildinfo.Version(version), runtime.Version())
}

// ---- client ----

// client is wt's own thin CLI wrapper. Its low-level checkVersion/get/status
// (http/base fields included) stay exactly as they've always been — the
// handshake and status-rendering behavior every existing test pins directly
// against this struct. The higher-level commands (ls/diff/review/approve/
// refresh/watch) are re-pointed onto cl, the shared internal/client.Client
// the TUI also uses (P3-design.md §2.3's "client extraction"): one place for
// the REST/SSE plumbing instead of two copies drifting apart.
type client struct {
	http *http.Client
	base string
	cl   *wtclient.Client
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
		cl: wtclient.New(socket),
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

func (c *client) ls() error {
	wts, err := c.cl.Worktrees(context.Background())
	if err != nil {
		return err
	}
	renderRadar(wts)
	return nil
}

func (c *client) watch() error {
	// Re-render on every event. Simple and correct; the fancy TUI would do partial
	// updates, but this proves the live stream end to end.
	render := func() {
		if wts, err := c.cl.Worktrees(context.Background()); err == nil {
			fmt.Print("\033[2J\033[H") // clear + home
			renderRadar(wts)
			fmt.Printf("\n%swatching — ctrl-c to exit%s\n", dim, reset)
		}
	}
	render()
	bell := &watchBell{out: os.Stdout, now: time.Now}
	events, errs := c.cl.Events(context.Background())
	for e := range events {
		bell.maybeRing(e)
		render()
	}
	return <-errs
}

// watchBellThrottle caps `wt watch`'s terminal bell to once per this long
// (P5-design.md §1.6) — an agent storm tripping several danger hits in a row
// must not turn the terminal into a klaxon.
const watchBellThrottle = 5 * time.Second

// watchBellBodyCap is the OSC 9 notification body's char cap — tighter than
// the desktop notifier's own 200 (P5-design.md §1.5): an OSC 9 popup has
// less room to show it.
const watchBellBodyCap = 120

// watchBell owns wt watch's "one bell per 5s" throttle across the event
// loop. Its own type (rather than a bare closure) is what makes the throttle
// logic unit-testable against a fake events channel and a fake clock, with
// no pty needed (P5-design.md WP2 test list).
type watchBell struct {
	out  io.Writer
	now  func() time.Time
	last time.Time
}

// maybeRing writes a BEL plus a sanitized OSC 9 notification to b.out when e
// is a danger-severity guardrail.tripped event and the throttle has
// elapsed since the last ring; every other event — including a danger hit
// arriving within the throttle window — is a no-op (P5-design.md §1.6).
// notify.Sanitize is the exact same rule the desktop notifier uses for its
// own argv content, reused here rather than a second, drifting
// implementation: no raw hit text can smuggle escape bytes into the
// terminal either way.
func (b *watchBell) maybeRing(e model.Event) {
	if e.Type != model.EventGuardrail || e.Hit == nil || e.Hit.Severity != "danger" {
		return
	}
	now := b.now()
	if !b.last.IsZero() && now.Sub(b.last) < watchBellThrottle {
		return
	}
	b.last = now
	msg := notify.Sanitize(e.Hit.Message, watchBellBodyCap)
	fmt.Fprintf(b.out, "\a\x1b]9;wt-cockpit: %s\x07", msg)
}

// shouldRenderSSELine advances the SSE per-frame event-name state machine (an
// "event: <name>" line names the frame that follows; a blank line ends it)
// and reports whether the just-scanned line is a "data:" payload that should
// trigger a re-render. Every data line renders except one inside a "hello"
// frame — the version-handshake preamble handleEvents sends first (see
// cmd/wtd's handleEvents/currentVersion).
//
// watch() itself no longer scans SSE lines by hand — it re-renders once per
// internal/client.Events delivery, which already applies this exact same
// hello-skip rule (see internal/client/sse.go) — but this function stays
// (and stays covered by its own tests below) as the historical pinned
// contract for that classification rule, byte-for-byte unchanged.
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
	d, err := c.cl.Diff(context.Background(), id)
	if err != nil {
		return err
	}
	renderDiff(d)
	return nil
}

// statusPayload mirrors /api/status's JSON shape. Kept local (like approve's
// anonymous result struct below) rather than importing the daemon's internal
// packages — wt only ever depends on the wire format, never on wtd's Go types.
type statusPayload struct {
	Version       string           `json:"version"`
	Protocol      int              `json:"protocol"`
	UptimeSeconds float64          `json:"uptimeSeconds"`
	SocketPath    string           `json:"socketPath"`
	WatcherMode   string           `json:"watcherMode"`
	Roots         []string         `json:"roots"`
	StatePath     string           `json:"statePath"`
	WebAddr       string           `json:"webAddr"` // "" when -web is off; see wt open below
	RepoCount     int              `json:"repoCount"`
	WorktreeCount int              `json:"worktreeCount"`
	ReviewedFiles int              `json:"reviewedFiles"`
	TotalFiles    int              `json:"totalFiles"`
	RulePacks     rulePacksPayload `json:"rulePacks"`
	Notifier      string           `json:"notifier"` // additive (P5-design.md §1.5, §2): the desktop notifier's resolved state
	// LastRefreshMs/LastRefreshOneMs are additive (P6-design.md §6.3 layer 3,
	// the v0.2 roadmap's "scan timings" IOU): the daemon's most recently
	// completed full Refresh / targeted RefreshOne wall-clock duration, in
	// milliseconds. 0 before either has completed once.
	LastRefreshMs    float64 `json:"lastRefreshMs"`
	LastRefreshOneMs float64 `json:"lastRefreshOneMs"`
}

// rulePacksPayload mirrors statusPayload's additive `rulePacks` field
// (P5-design.md §1.3, §2): how many currently-known repos are running a
// validly-loaded .wtcockpit.toml pack vs a malformed one that fell back to
// global rules.
type rulePacksPayload struct {
	Loaded int `json:"loaded"`
	Errors int `json:"errors"`
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
	if err := c.cl.SetReviewed(context.Background(), id, file, reviewed, ""); err != nil {
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
	res, err := c.cl.Approve(context.Background(), id)
	if err != nil {
		return err
	}
	fmt.Printf("%s✓ merged %s → %s%s and removed the worktree\n", green, res.Merged, res.Into, reset)
	return nil
}

// comments lists id's comments. --json is the frozen agent-integration
// contract (P4-design.md §1.5): the daemon's model.CommentsPayload is decoded
// then re-emitted with MarshalIndent verbatim, so the API and the CLI can
// never drift apart. Default is open-only; --all requests every state.
func (c *client) comments(id string, jsonOut, all bool) error {
	state := "open"
	if all {
		state = "all"
	}
	payload, err := c.cl.Comments(context.Background(), id, state, "")
	if err != nil {
		return err
	}
	if jsonOut {
		b, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	renderComments(payload)
	return nil
}

// comment adds one, printing the daemon-assigned id (the handle `wt resolve`
// later needs).
func (c *client) comment(id, file string, line int, side, author, body string) error {
	cm, err := c.cl.AddComment(context.Background(), id, file, line, side, body, author)
	if err != nil {
		return err
	}
	fmt.Printf("added comment %s on %s:%d in %s\n", cm.ID, cm.File, cm.Line, id)
	return nil
}

// resolve is the agent's loop-closer — CLI deliberately has no --delete
// (P4-design.md §1.5: mistakes via CLI get deleted in the web UI later).
func (c *client) resolve(id, commentID string) error {
	if err := c.cl.ResolveComment(context.Background(), id, commentID); err != nil {
		return err
	}
	fmt.Printf("resolved %s in %s\n", commentID, id)
	return nil
}

// rules shows the effective, provenance-tagged rule set for a worktree's
// owning repo (P5-design.md §1.3) — the precedence-confusion antidote: one
// command answers "why did/didn't this rule fire", including whether a
// per-repo .wtcockpit.toml pack is in play (and if it's malformed).
func (c *client) rules(id string, jsonOut bool) error {
	eff, err := c.cl.Rules(context.Background(), id)
	if err != nil {
		return err
	}
	if jsonOut {
		b, err := json.MarshalIndent(eff, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	renderRules(eff)
	return nil
}

// open resolves wtd's web listener address via /api/status (empty when -web
// is off) and launches the browser at that worktree's reading-room URL
// (P4-design.md §1.6). Reuses c.get/statusPayload (like status/renderStatus
// above) rather than the shared internal/client.Client — /api/status has no
// method there yet, and one more raw GET here doesn't earn adding one.
func (c *client) open(id string) error {
	var st statusPayload
	if err := c.get("/api/status", &st); err != nil {
		return err
	}
	if st.WebAddr == "" {
		return errors.New("wtd is not serving the web UI — start it with -web 127.0.0.1:7788")
	}
	return openBrowser(roomURL(st.WebAddr, id))
}

// roomURL builds a worktree's reading-room URL off wtd's actual bound web
// address (correct even under an ephemeral "-web 127.0.0.1:0"). Shared by
// open (above) and menubar's per-worktree rows (below) — one place building
// this URL shape rather than two that could drift.
func roomURL(webAddr, id string) string {
	return fmt.Sprintf("http://%s/wt/%s", webAddr, url.PathEscape(id))
}

// browserCommand picks which program to launch for openBrowser, in priority
// order: $BROWSER, then the platform opener. goos is runtime.GOOS, passed in
// so this selection logic is unit-testable without actually executing
// anything platform-specific. "" (no BROWSER, an unrecognised goos) tells
// the caller to just print the URL instead of exec'ing.
func browserCommand(browserEnv, goos string) string {
	if browserEnv != "" {
		return browserEnv
	}
	switch goos {
	case "darwin":
		return "open"
	case "linux":
		return "xdg-open"
	default:
		return ""
	}
}

// openBrowser launches url via browserCommand's pick, non-blocking (Start,
// not Run) — $BROWSER may name a raw, non-forking browser binary that would
// otherwise hang wt for as long as the browser stays open. $BROWSER-first is
// also what makes this scriptably testable end to end: `BROWSER=echo wt open
// <id>` prints the exact URL (its stdout/stderr are wired to wt's own).
func openBrowser(url string) error {
	prog := browserCommand(os.Getenv("BROWSER"), runtime.GOOS)
	if prog == "" {
		fmt.Println(url)
		return nil
	}
	cmd := exec.Command(prog, url)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Start()
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
	// alerts is a TOTAL-hits count (every worktree's every guardrail hit), the
	// same definition the web index/CLI already used — ux-expert P2-3 unified
	// the TUI topbar (which used to count worktrees-with-a-hit instead) onto
	// this one, so "alerts" means the same number everywhere.
	var active, alerts int
	for _, w := range wts {
		if w.State == model.StateActive {
			active++
		}
		alerts += len(w.Guardrails)
	}
	fmt.Printf("%s%swt cockpit%s  %s%d worktrees · %d active · %d alerts%s\n\n",
		bold, blue, reset, dim, len(wts), active, alerts, reset)

	if len(wts) == 0 {
		fmt.Printf("%sno worktrees — wtd -root <dir> or edit ~/.config/wtcockpit/config.toml%s\n\n", dim, reset)
	}

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
		// repo/w.Name are filesystem-derived (repo dir / worktree dir basename)
		// and can carry raw control bytes on Unix (P7 security LOW-1) — sanitized
		// here, at the render sink, the same treatment shortGuard's rule names
		// and the web/menubar renderers of this exact untrusted text already get.
		fmt.Printf("%s%s%s\n", faint, diffparse.SanitizeControl(repo), reset)
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
				white, truncate(diffparse.SanitizeControl(w.Name), 24), reset,
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

// shortGuard renders a worktree's guardrail hits as a compact "rule, rule,
// …" summary. h.Rule is a rule NAME, which can come from a semi-trusted
// pack's own [[rules]] Name field (HIGH security fix) — sanitized here via
// diffparse.SanitizeControl, the same rule the TUI/web guardrail banners
// already apply, before it ever reaches renderRadar's terminal output.
func shortGuard(hits []model.GuardrailHit) string {
	seen := map[string]bool{}
	var parts []string
	for _, h := range hits {
		name := diffparse.SanitizeControl(h.Rule)
		if !seen[name] {
			seen[name] = true
			parts = append(parts, name)
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

func renderComments(p model.CommentsPayload) {
	fmt.Printf("%s%scomments%s  %s%s%s  %s%d%s\n", bold, blue, reset, dim, p.WorktreeID, reset, dim, len(p.Comments), reset)
	for _, cm := range p.Comments {
		flag := ""
		switch {
		case cm.Orphaned:
			flag = amber + " [orphaned]" + reset
		case cm.Stale:
			flag = amber + " [stale]" + reset
		}
		fmt.Printf("  %s%s%s %s%s:%d (%s)%s%s\n", white, cm.ID, reset, dim, cm.File, cm.Line, cm.State, reset, flag)
		fmt.Printf("    %s%s%s  %s— %s%s\n", reset, cm.Body, reset, faint, cm.Author, reset)
	}
	if len(p.Comments) == 0 {
		fmt.Printf("  %s(none)%s\n", dim, reset)
	}
}

// renderRules is `wt rules <id>`'s human table: NAME SEV SOURCE CONDITIONS
// MESSAGE, plus the pack file's own path+status when one is in effect — the
// precedence-confusion antidote named in P5-design.md §1.3.
//
// Every one of Name/Message/PackStatus (and, via conditionsSummary,
// PathGlob/AddedPattern/etc.) can carry a semi-trusted main-worktree pack's
// own text (HIGH security fix): a hostile .wtcockpit.toml rule's Message
// using a TOML backslash-u escape can decode to real OSC/ANSI bytes, which
// this terminal renderer must neutralize before printing — the same
// treatment the TUI/web guardrail banners already give this exact input via
// diffparse.SanitizeControl.
func renderRules(eff guardrail.Effective) {
	fmt.Printf("%s%srules%s  %s%s%s\n", bold, blue, reset, dim, eff.RepoPath, reset)
	if eff.PackPath == "" {
		fmt.Printf("  %spack%s  (none)\n", dim, reset)
	} else {
		fmt.Printf("  %spack%s  %s (%s)\n", dim, reset, eff.PackPath, diffparse.SanitizeControl(eff.PackStatus))
	}
	fmt.Println()
	fmt.Printf("  %-24s %-7s %-7s %-40s %s\n", "NAME", "SEV", "SOURCE", "CONDITIONS", "MESSAGE")
	for _, r := range eff.Rules {
		fmt.Printf("  %-24s %-7s %-7s %-40s %s\n",
			truncate(diffparse.SanitizeControl(r.Name), 24), effectiveSeverity(r.Severity), r.Source,
			truncate(conditionsSummary(r.Rule), 40), diffparse.SanitizeControl(r.Message))
	}
}

// effectiveSeverity mirrors guardrail's own severityOrDefault (unexported
// there): a rule's blank Severity means "warn" everywhere it's evaluated.
func effectiveSeverity(s string) string {
	if s == "" {
		return "warn"
	}
	return s
}

// conditionsSummary renders a Rule's set condition fields as a compact,
// greppable one-liner — cosmetic only (cmd/wt holds no guardrail logic of
// its own; this just formats the wire type for display). Sanitized as a
// whole before return: several of these fields (path_glob(s), added_pattern)
// are semi-trusted pack-authored text (HIGH security fix), same as Name/
// Message above.
func conditionsSummary(r guardrail.Rule) string {
	var parts []string
	add := func(format string, args ...any) { parts = append(parts, fmt.Sprintf(format, args...)) }

	if r.PathGlob != "" {
		add("path_glob=%s", r.PathGlob)
	}
	if len(r.PathGlobs) > 0 {
		add("path_globs=%s", strings.Join(r.PathGlobs, ","))
	}
	if len(r.ExcludeGlobs) > 0 {
		add("exclude_globs=%s", strings.Join(r.ExcludeGlobs, ","))
	}
	if r.Status != "" {
		add("status=%s", r.Status)
	}
	if r.Binary {
		add("binary")
	}
	if r.MinNetDeleted > 0 {
		add("min_net_deleted=%d", r.MinNetDeleted)
	}
	if r.MinChangedLines > 0 {
		add("min_changed_lines=%d", r.MinChangedLines)
	}
	if r.AddedPattern != "" {
		add("added_pattern=%s", r.AddedPattern)
	}
	if r.MinTokenEntropy > 0 {
		add("min_token_entropy=%.1f,min_token_len=%d", r.MinTokenEntropy, r.MinTokenLen)
	}
	if r.MinDeleteAddRatio > 0 {
		add("min_delete_add_ratio=%.1f", r.MinDeleteAddRatio)
	}
	if r.MinFilesChanged > 0 {
		add("min_files_changed=%d", r.MinFilesChanged)
	}
	if r.MinTotalChanged > 0 {
		add("min_total_changed=%d", r.MinTotalChanged)
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return diffparse.SanitizeControl(strings.Join(parts, " "))
}

// ---- menubar: SwiftBar/xbar plugin emitter (P5-design.md §1.7) ----

// renderMenubar writes wt menubar's plugin text to w: the title line, one
// row per worktree needing attention, and a trailing "Open cockpit" link —
// or, when wtd can't be reached (or answers with a stale/incompatible
// handshake), the minimal degraded block ("wt ◦"). Never returns an error:
// a plugin host treats a nonzero wt exit as a broken plugin, and "the daemon
// is down" is an ordinary, expected state to render instead of failing on.
func renderMenubar(w io.Writer, c *client) {
	if err := c.checkVersion(); err != nil {
		writeMenubarDown(w)
		return
	}
	wts, err := c.cl.Worktrees(context.Background())
	if err != nil {
		writeMenubarDown(w)
		return
	}
	// webAddr absent — -web off, P4 not merged, or /api/status erroring on an
	// otherwise-healthy daemon — just means rows/links render without an
	// href (§1.7's "-web off" degradation); it does not warrant the full
	// "wtd unreachable" state, since the fleet data above is good.
	var st statusPayload
	_ = c.get("/api/status", &st)

	writeMenubar(w, wts, st.WebAddr)
}

func writeMenubarDown(w io.Writer) {
	fmt.Fprintln(w, "wt ◦")
	fmt.Fprintln(w, "---")
	fmt.Fprintln(w, "wtd not reachable — is it running?")
}

// writeMenubar formats the SwiftBar/xbar plugin text itself (P5-design.md
// §1.7's frozen example): title = danger-worktree count (not total danger
// HIT count — one worktree with several danger hits still counts once) plus
// reviewed/total files fleet-wide, then one row per worktree that needs
// attention, then a trailing "Open cockpit" link. Rows/links carry an href
// only when webAddr is non-empty — the same degradation `wt open` already
// applies when -web is off.
func writeMenubar(w io.Writer, wts []model.Worktree, webAddr string) {
	sorted := append([]model.Worktree(nil), wts...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Repo != sorted[j].Repo {
			return sorted[i].Repo < sorted[j].Repo
		}
		return sorted[i].Name < sorted[j].Name
	})

	dangerWTs, reviewed, total := 0, 0, 0
	for _, wt := range sorted {
		total += wt.Stats.Files
		reviewed += wt.Reviewed
		if dangerCount(wt.Guardrails) > 0 {
			dangerWTs++
		}
	}
	fmt.Fprintf(w, "⚠%d ✓%d/%d\n", dangerWTs, reviewed, total)
	fmt.Fprintln(w, "---")

	for _, wt := range sorted {
		label := menubarLabel(wt)
		if label == "" {
			continue // fully reviewed, no danger hit: nothing to draw attention to
		}
		line := sanitizeMenubarField(wt.Repo) + "/" + sanitizeMenubarField(wt.Name) + " — " + label
		if webAddr != "" {
			line += " | href=" + roomURL(webAddr, wt.ID)
		}
		fmt.Fprintln(w, line)
	}

	fmt.Fprintln(w, "---")
	openRow := "Open cockpit"
	if webAddr != "" {
		openRow += " | href=http://" + webAddr + "/"
	}
	fmt.Fprintln(w, openRow)
}

// sanitizeMenubarField makes worktree-derived text (a repo directory name, a
// branch name) safe to embed in a SwiftBar/xbar plugin line (BLOCKER-1
// security fix): git permits a branch name to contain "|", and SwiftBar/xbar
// splits a line on the FIRST "|" into title|params, where params include
// bash=/shell= (run a command on click) or href= (open a URL) — SwiftBar has
// no escape for a literal "|", so an agent-chosen branch name like
// "feat|bash=/tmp/evil.sh" would otherwise become a clickable run-on-click
// row for whoever installed the plugin. A repo directory name is
// filesystem-derived and can carry arbitrary bytes, including raw control
// bytes/newlines, on some platforms — diffparse.SanitizeControl (the same
// rule every other renderer of untrusted worktree text already uses) turns
// those into visible caret notation before the "|" substitution.
func sanitizeMenubarField(s string) string {
	return strings.ReplaceAll(diffparse.SanitizeControl(s), "|", "¦")
}

// menubarLabel is one worktree's row text, or "" when it needs no attention
// at all (fully reviewed, no danger hit) — such a worktree gets no row,
// holding to the same "only what's noteworthy" bar the title's own
// danger-worktree count applies. A danger hit takes priority over an
// unreviewed-files count when a worktree has both.
func menubarLabel(wt model.Worktree) string {
	switch {
	case dangerCount(wt.Guardrails) > 0:
		return fmt.Sprintf("%d danger", dangerCount(wt.Guardrails))
	case wt.Reviewed < wt.Stats.Files:
		return fmt.Sprintf("%d files unreviewed", wt.Stats.Files-wt.Reviewed)
	default:
		return ""
	}
}

func dangerCount(hits []model.GuardrailHit) int {
	n := 0
	for _, h := range hits {
		if h.Severity == "danger" {
			n++
		}
	}
	return n
}

func renderStatus(st statusPayload) {
	uptime := time.Duration(st.UptimeSeconds * float64(time.Second)).Round(time.Second)
	fmt.Printf("%s%swtd status%s\n", bold, blue, reset)
	fmt.Printf("  %sversion%s    %s (protocol %d)\n", dim, reset, st.Version, st.Protocol)
	fmt.Printf("  %suptime%s     %s\n", dim, reset, uptime)
	fmt.Printf("  %ssocket%s     %s\n", dim, reset, st.SocketPath)
	web := st.WebAddr
	if web == "" {
		web = "(off)"
	}
	fmt.Printf("  %sweb%s        %s\n", dim, reset, web)
	fmt.Printf("  %swatcher%s    %s\n", dim, reset, st.WatcherMode)
	fmt.Printf("  %sstate%s      %s\n", dim, reset, st.StatePath)
	fmt.Printf("  %sroots%s      %s\n", dim, reset, strings.Join(st.Roots, ", "))
	fmt.Printf("  %srepos%s      %d\n", dim, reset, st.RepoCount)
	fmt.Printf("  %sworktrees%s  %d\n", dim, reset, st.WorktreeCount)
	fmt.Printf("  %sreviewed%s   %d/%d files\n", dim, reset, st.ReviewedFiles, st.TotalFiles)
	fmt.Printf("  %srule packs%s %d loaded, %d errors\n", dim, reset, st.RulePacks.Loaded, st.RulePacks.Errors)
	fmt.Printf("  %snotifier%s   %s\n", dim, reset, st.Notifier)
	fmt.Printf("  %srefresh (full)%s %s\n", dim, reset, formatRefreshMs(st.LastRefreshMs))
	fmt.Printf("  %srefresh (one)%s  %s\n", dim, reset, formatRefreshMs(st.LastRefreshOneMs))
}

// formatRefreshMs renders a statusPayload refresh-timing field (P6-design.md
// §6.3 layer 3): 0 means the daemon hasn't completed that kind of refresh
// yet (rather than a genuinely instantaneous one — indistinguishable from 0
// in practice, and "none yet" is the far more common reason to see it),
// otherwise one decimal place of milliseconds.
func formatRefreshMs(ms float64) string {
	if ms <= 0 {
		return "n/a (none yet)"
	}
	return fmt.Sprintf("%.1fms", ms)
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

  wt                        on a terminal: full-screen TUI (same as wt tui);
                             piped/redirected: text radar (same as wt ls)
  wt tui                    full-screen cockpit: radar + review panes, live
  wt ls                     list worktrees (radar)
  wt watch                  live radar, updates on every change
  wt diff <id>              show a worktree's diff
  wt status [--json]        daemon health: version, uptime, watcher, roots, review counts
  wt review <id> <file>     mark a file reviewed (--off to unmark)
  wt approve <id>           merge worktree→base & remove it (needs full review + clean tree)
  wt refresh                force a rescan
  wt comments <id> [--json] [--all]
                             list comments (default: open only; --json is the
                             frozen agent-integration contract)
  wt comment <id> <file> <line> [--old] [--author <name>] <body...>
                             add a comment (line 0 = file-level; --old for the base-side line)
  wt resolve <id> <comment-id>
                             mark a comment resolved
  wt open <id>               open the worktree's reading room in a browser
                             (needs wtd started with -web; $BROWSER wins if set)
  wt rules <id> [--json]    show the effective guardrail rules for a worktree's
                             repo, with provenance (default/global/pack) and
                             any .wtcockpit.toml pack's status
  wt menubar                 SwiftBar/xbar plugin text (danger-worktree count,
                             reviewed/total files, per-worktree rows) —
                             installed as an xbar plugin, not run by hand
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
