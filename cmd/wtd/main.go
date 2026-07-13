// Command wtd is the cockpit daemon. It runs the engine and exposes it over an
// HTTP API served on a Unix socket (and optionally TCP for remote/Tailscale use).
// The same handler powers the TUI client, the web reading room, and a future
// menu-bar app — every frontend is a thin client of this one process.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/navbytes/wt-cockpit/internal/buildinfo"
	"github.com/navbytes/wt-cockpit/internal/config"
	"github.com/navbytes/wt-cockpit/internal/engine"
	"github.com/navbytes/wt-cockpit/internal/gitbackend"
	"github.com/navbytes/wt-cockpit/internal/guardrail"
	"github.com/navbytes/wt-cockpit/internal/model"
	"github.com/navbytes/wt-cockpit/internal/notify"
	"github.com/navbytes/wt-cockpit/internal/registry"
	"github.com/navbytes/wt-cockpit/internal/store"
	"github.com/navbytes/wt-cockpit/internal/watcher"
	"github.com/navbytes/wt-cockpit/internal/web"
)

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*m = append(*m, p)
		}
	}
	return nil
}

// version is set at build time via -ldflags "-X main.version=...". "dev" is
// the fallback for a plain `go build`/`go run`.
var version = "dev"

func main() {
	var roots multiFlag
	home, _ := os.UserHomeDir()
	dataDir := filepath.Join(home, ".wtcockpit")

	flag.Var(&roots, "root", "root directory to scan for repos (repeatable, comma-ok)")
	socket := flag.String("socket", filepath.Join(dataDir, "wtd.sock"), "unix socket path")
	tcp := flag.String("tcp", "", "optional TCP address to also listen on (e.g. 127.0.0.1:7799)")
	webAddr := flag.String("web", "", "optional loopback web UI address (e.g. 127.0.0.1:7788); refuses non-loopback binds (remote access is v0.7)")
	statePath := flag.String("state", filepath.Join(dataDir, "state.db"), "review state file (.db → SQLite, default; .json → legacy JSON store)")
	base := flag.String("base", "", "diff baseline branch (default: each repo's own default)")
	interval := flag.Duration("interval", 2*time.Second, "poll interval (also governs the fsnotify reconciliation tick)")
	watchMode := flag.String("watch", "fsnotify", "watcher backend: fsnotify (default) or poll")
	configPath := flag.String("config", config.DefaultPath(), "path to config.toml (roots, base, guardrails, daemon options)")
	logFormat := flag.String("log-format", "text", "log output format: text or json")
	logLevel := flag.String("log-level", "info", "log verbosity: debug, info, warn, or error")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		printVersion(os.Stdout)
		return
	}

	// Precedence is explicit flags > config file > the built-in defaults set
	// above. flag.Visit only calls back for flags the user actually passed, so
	// it's how we tell "explicit -base main" apart from "-base defaulted to ''".
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	// -config was explicitly requested: unlike the conventional default path, a
	// missing or invalid file here is fatal — the user asked for this file.
	if explicit["config"] {
		if _, statErr := os.Stat(*configPath); statErr != nil {
			slog.Error("config file not found", "path", *configPath, "error", statErr)
			os.Exit(1)
		}
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config failed", "path", *configPath, "error", err)
		os.Exit(1)
	}

	roots = mergeRoots(roots, explicit["root"], cfg.Roots)
	*base = mergeSetting(*base, explicit["base"], cfg.Base)
	*socket = mergeSetting(*socket, explicit["socket"], cfg.Socket)
	*tcp = mergeSetting(*tcp, explicit["tcp"], cfg.TCP)
	*webAddr = mergeSetting(*webAddr, explicit["web"], cfg.Web)
	*statePath = mergeSetting(*statePath, explicit["state"], cfg.State)
	*watchMode = mergeSetting(*watchMode, explicit["watch"], cfg.Watch)
	*logFormat = mergeSetting(*logFormat, explicit["log-format"], cfg.LogFormat)
	*logLevel = mergeSetting(*logLevel, explicit["log-level"], cfg.LogLevel)

	// Unknown values are refused, same as a malformed config file: silently
	// running at the wrong verbosity or format is worse than a restart.
	if *logFormat != "text" && *logFormat != "json" {
		fmt.Fprintf(os.Stderr, "wtd: invalid log-format %q (want text or json)\n", *logFormat)
		os.Exit(1)
	}
	switch *logLevel {
	case "debug", "info", "warn", "error":
	default:
		fmt.Fprintf(os.Stderr, "wtd: invalid log-level %q (want debug, info, warn, or error)\n", *logLevel)
		os.Exit(1)
	}

	// The logger must exist before clampInterval's possible warning below (and
	// everything else in main), so it's set up as soon as the settings it
	// itself depends on (log-format/log-level) are resolved.
	slog.SetDefault(newLogger(*logFormat, *logLevel, os.Stderr))

	// -web is loopback-only, full stop: remote access to a listener that
	// renders agent-authored bytes into a browser is a v0.7 problem (auth),
	// not this phase's. Validated as soon as the logger exists so the
	// refusal message goes through the normal slog path like every other
	// startup failure below.
	if *webAddr != "" {
		if err := validateWebAddr(*webAddr); err != nil {
			slog.Error("invalid -web address", "error", err)
			os.Exit(1)
		}
	}

	*interval = clampInterval(mergeSetting(*interval, explicit["interval"], cfg.Interval))

	if len(roots) == 0 {
		if cwd, err := os.Getwd(); err == nil {
			roots = multiFlag{cwd}
		}
	}
	_ = os.MkdirAll(dataDir, 0o755)

	st, err := store.Open(*statePath)
	if err != nil {
		slog.Error("open state failed", "path", *statePath, "error", err)
		os.Exit(1)
	}
	slog.Info("state store", "backend", backendName(*statePath), "path", *statePath)
	reg := registry.New()
	// globalSource distinguishes DefaultRules() from a user's own config
	// [[rules]] for /api/rules and `wt rules`'s provenance column
	// (P5-design.md §1.3) — config.Load already routed cfg.Rules through
	// guardrail.Compile once for the "typo fails fast" check, so this second
	// Compile (inside NewResolver) is defensive, not load-bearing.
	globalSource := "default"
	if cfg.RulesSet {
		globalSource = "global"
	}
	gr, err := guardrail.NewResolver(cfg.RulesOr(guardrail.DefaultRules()), globalSource)
	if err != nil {
		slog.Error("invalid guardrail rules", "error", err)
		os.Exit(1)
	}
	be := gitbackend.NewCLI()
	eng := engine.New(engine.Config{
		Roots:          roots,
		DefaultBase:    *base,
		BaseFor:        baseFor(cfg),
		ActivityWindow: 30 * time.Second,
	}, be, reg, st, gr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Watcher drives refreshes. fsnotify is git-state-first and default; -watch
	// poll keeps the old fixed-interval fallback (e.g. for flaky network mounts).
	var wch watcher.Watcher
	switch *watchMode {
	case "fsnotify":
		wch = &watcher.FSWatcher{Roots: roots, Interval: *interval, Backend: be}
	case "poll":
		wch = &watcher.Poller{Interval: *interval}
	default:
		slog.Error("unknown -watch value", "watch", *watchMode, "want", "fsnotify or poll")
		os.Exit(1)
	}
	go wch.Run(ctx, func(path string) {
		var err error
		if path == "" {
			err = eng.Refresh(ctx)
		} else {
			err = eng.RefreshOne(ctx, path)
		}
		if err != nil && ctx.Err() == nil {
			slog.Error("refresh failed", "error", err)
		}
	})

	// The desktop notifier is just another registry-bus subscriber (P5-
	// design.md §1.5), constructed unconditionally (cheap: New probes PATH
	// only when cfg.Notifications is actually enabled) so its Status() is
	// always available for /api/status regardless of the enabled/disabled
	// state. The subscription itself is only created when enabled — "enabled
	// = false skips the subscription entirely" — a disabled-by-config
	// notifier never even occupies a registry channel slot.
	notifier := notify.New(notify.Config{
		Enabled:  cfg.Notifications.EnabledOr(),
		Severity: cfg.Notifications.SeverityOr(),
		Cooldown: cfg.Notifications.CooldownOr(),
	}, notifyLookup(reg))
	if cfg.Notifications.EnabledOr() {
		notifyCh, notifyCancel := reg.Subscribe(256)
		go func() {
			notifier.Run(ctx, notifyCh)
			notifyCancel()
		}()
	}

	// The web listener is opened before srv exists (and so before any
	// handler goroutine can start reading srv.webAddr concurrently): doing
	// it here is also what resolves an ephemeral "127.0.0.1:0" -web bind to
	// its real, effective port, which /api/status must then report exactly
	// (P4-design.md §1.1, §1.6).
	var webLn net.Listener
	var resolvedWebAddr string
	if *webAddr != "" {
		var werr error
		webLn, werr = net.Listen("tcp", *webAddr)
		if werr != nil {
			slog.Error("listen web failed", "web", *webAddr, "error", werr)
			os.Exit(1)
		}
		resolvedWebAddr = webLn.Addr().String()
	}

	srv := &server{
		eng:         eng,
		socketPath:  *socket,
		watcherMode: *watchMode,
		roots:       roots,
		statePath:   *statePath,
		startedAt:   time.Now(),
		webAddr:     resolvedWebAddr,
		notifier:    notifier,
	}
	handler := srv.routes()

	// Listen on the unix socket (primary transport).
	_ = os.Remove(*socket)
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		slog.Error("listen unix failed", "socket", *socket, "error", err)
		os.Exit(1)
	}
	defer os.Remove(*socket)
	slog.Info("wtd starting", "version", version, "socket", *socket, "roots", roots.String(), "watcherMode", *watchMode, "statePath", *statePath)

	// Three listeners can now share the one API mux (handler), each with its
	// own http.Server because each wants different middleware wrapped
	// around the identical, unmodified handler value (P4-design.md §1.1):
	// the socket stays completely tokenless (filesystem perms are its trust
	// boundary, as always), -tcp gains Host+Origin validation, and -web gets
	// the full browser-security stack plus the served pages.
	socketSrv := &http.Server{Handler: handler}
	go func() { _ = socketSrv.Serve(ln) }()

	var tcpSrv *http.Server
	if *tcp != "" {
		tln, err := net.Listen("tcp", *tcp)
		if err != nil {
			slog.Error("listen tcp failed", "tcp", *tcp, "error", err)
			os.Exit(1)
		}
		tcpSrv = &http.Server{Handler: web.HostOriginOnly(handler, *tcp)}
		slog.Info("wtd also listening", "tcp", *tcp)
		go func() { _ = tcpSrv.Serve(tln) }()
	}

	var webSrv *http.Server
	if webLn != nil {
		webSrv = &http.Server{
			Handler: web.New(eng, handler, web.Config{
				BoundAddr: resolvedWebAddr,
				CSRFToken: web.NewCSRFToken(),
				Roots:     roots,
			}),
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       120 * time.Second,
			// No WriteTimeout: it would kill the SSE stream.
		}
		slog.Info("wtd also listening", "web", resolvedWebAddr)
		go func() { _ = webSrv.Serve(webLn) }()
	}

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = socketSrv.Shutdown(shutCtx)
	if tcpSrv != nil {
		_ = tcpSrv.Shutdown(shutCtx)
	}
	if webSrv != nil {
		_ = webSrv.Shutdown(shutCtx)
	}
	// Store isn't part of the Store interface (sqlite.go's sqliteStore.Close
	// deliberately keeps it off — jsonStore has no handle to release); this
	// type-asserts for it so a SQLite-backed daemon checkpoints its WAL on a
	// clean shutdown, while a JSON-backed one (no Close method) is a no-op.
	if closer, ok := st.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	slog.Info("wtd stopped")
}

// backendName reports which Store backend -state's extension selects
// (store.Open's own dispatch rule, P6-design.md §7) — purely for the
// startup log line naming what's actually running.
func backendName(path string) string {
	if filepath.Ext(path) == ".json" {
		return "json"
	}
	return "sqlite"
}

// printVersion writes the build-time version string to w. Pulled out of the
// -version flag branch (same rationale as mergeSetting/clampInterval living
// outside main) so the flag's actual behaviour is unit-testable without
// exercising flag.Parse or os.Exit.
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "wtd %s (%s)\n", buildinfo.Version(version), runtime.Version())
}

// newLogger builds the slog.Logger wtd runs with for the rest of its life. w
// is os.Stderr in main; taking it as a parameter (like printVersion takes an
// io.Writer) is what makes -log-format json testable without redirecting the
// process's real stderr. slog.SetDefault makes the result the target both for
// slog's own top-level functions and for anything still logging via the
// standard "log" package — including internal/watcher's warnings and any
// future dependency that only knows about log.Print (see
// TestSlogSetDefaultBridgesStandardLogPackage).
func newLogger(format, level string, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLogLevel(level)}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

// parseLogLevel maps the -log-level flag/config value to a slog.Level. An
// unrecognised value (unreachable after main's validation) falls back to Info
// rather than refusing to start over a logging nicety.
func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// mergeSetting resolves one setting's final value under flags > config >
// built-in-default precedence. flagVal is the flag variable's value after
// flag.Parse: the user's explicit value if explicit is true, otherwise
// whatever built-in default it was registered with. cfgVal is the config
// file's value for the same setting (""/zero means the config didn't set it).
//
// NT1: this can't tell "config explicitly set the value to T's zero value"
// (e.g. interval = "0s") apart from "config never mentioned this setting" —
// both look identical (cfgVal == zero) and both fall back to flagVal. Fine
// for every setting wtd currently merges this way (an explicit zero isn't a
// meaningful choice for any of them), but worth knowing before reusing this
// for a setting where zero is a legitimate, distinct value from unset.
func mergeSetting[T comparable](flagVal T, explicit bool, cfgVal T) T {
	if explicit {
		return flagVal
	}
	var zero T
	if cfgVal != zero {
		return cfgVal
	}
	return flagVal
}

// clampInterval enforces a practical floor on the poll/reconciliation
// interval (NT2): a tiny but positive value — e.g. a "10ms" typo in a flag or
// config file — would peg a CPU core on a full-rescan loop, so anything below
// 1s is raised to it, with a warning so the operator knows why what they
// configured isn't what's running. Zero and negative values are left alone:
// they already have their own documented fallback in the watcher package
// (Poller and FSWatcher both default to a sane interval on interval<=0), and
// clamping them here too would just duplicate that with a different constant.
// This lives here, not in the watcher constructors, because their tests
// deliberately use sub-second intervals for speed.
func clampInterval(d time.Duration) time.Duration {
	const floor = time.Second
	if d > 0 && d < floor {
		slog.Warn("interval below floor; clamping", "interval", d, "floor", floor)
		return floor
	}
	return d
}

// validateWebAddr enforces -web's loopback-only bind (P4-design.md §1.1):
// the host half of addr must be 127.0.0.0/8, ::1, or the literal "localhost"
// — anything else (0.0.0.0, a public/LAN IP, an arbitrary hostname, or a
// malformed address) is refused with a message naming exactly why and
// pointing at the version that lifts the restriction, matching this
// package's other "refuse loudly, name the value, say what to do" startup
// checks (see e.g. the -log-format/-log-level validation above). Port 0 is
// deliberately allowed here (ephemeral bind, e.g. for tests) — only the host
// is judged.
func validateWebAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || !isLoopbackHost(host) {
		return fmt.Errorf("-web must bind loopback (got %q); remote access is v0.7", addr)
	}
	return nil
}

// isLoopbackHost reports whether host (the host half of a "host:port"
// address, so no brackets around an IPv6 literal) names loopback: the
// literal "localhost", or an IP that net.IP.IsLoopback agrees is loopback
// (127.0.0.0/8 or ::1). An empty host (as in ":7788", which net.Listen
// treats as "every interface") is never loopback.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// mergeRoots applies the same flags > config > built-in-default precedence
// for -root, except an explicit -root *replaces* config roots entirely rather
// than merging with them — no surprise unions of CLI and config roots.
func mergeRoots(flagVal []string, explicit bool, cfgVal []string) []string {
	if explicit {
		return flagVal
	}
	if len(cfgVal) > 0 {
		return cfgVal
	}
	return flagVal
}

// baseFor builds the engine's per-repo base-branch override map from the
// config's [repos."<path>"] tables, dropping entries that don't set a base.
// Keys are normalised (~-expanded, then made absolute) the same way
// discovery resolves each root before turning it into a repo.Path — the two
// must match exactly, or a `[repos."~/code/x"]` override silently no-ops.
func baseFor(cfg config.Config) map[string]string {
	if len(cfg.Repos) == 0 {
		return nil
	}
	m := make(map[string]string, len(cfg.Repos))
	for path, rc := range cfg.Repos {
		if rc.Base != "" {
			m[cleanRepoKey(path)] = rc.Base
		}
	}
	return m
}

// cleanRepoKey mirrors discovery's own root normalisation (~-expansion +
// filepath.Abs; discovery.expandHome isn't exported, so this is a minimal
// local equivalent) so a config [repos."..."] key compares equal to the
// repo.Path discovery actually produces.
func cleanRepoKey(path string) string {
	if abs, err := filepath.Abs(expandHome(path)); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// expandHome expands a leading "~" (or "~/...") to the user's home directory,
// same convention as discovery.expandHome.
func expandHome(p string) string {
	if len(p) >= 1 && p[0] == '~' && (len(p) == 1 || p[1] == '/') {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// notifyLookup adapts the registry's Get into notify.Lookup: the notifier
// resolves a worktree id to its repo/name for notification titles without
// ever importing the engine (P5-design.md §1.5).
func notifyLookup(reg *registry.Registry) notify.Lookup {
	return func(id string) (repo, name string, ok bool) {
		w := reg.Get(id)
		if w == nil {
			return "", "", false
		}
		return w.Repo, w.Name, true
	}
}

// server holds everything an HTTP handler needs. socketPath/watcherMode/
// roots/statePath/startedAt only exist for /api/status to report — main is
// otherwise the sole owner of those settings, as resolved flags. notifier is
// nil in tests that build a *server literal directly without going through
// main (see handleStatus's nil guard) — every production server always has
// one (main always constructs it, even when notifications are disabled).
type server struct {
	eng         *engine.Engine
	socketPath  string
	watcherMode string
	roots       []string
	statePath   string
	startedAt   time.Time
	webAddr     string // "" when -web is off; else the actual bound address (correct under port 0)
	notifier    *notify.Notifier
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/api/version", s.handleVersion)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/worktrees", s.handleWorktrees)
	mux.HandleFunc("/api/rules", s.handleRules)
	mux.HandleFunc("/api/diff", s.handleDiff)
	mux.HandleFunc("/api/review", s.handleReview)
	mux.HandleFunc("/api/approve", s.handleApprove)
	mux.HandleFunc("/api/refresh", s.handleRefresh)
	mux.HandleFunc("/api/comments", s.handleComments)
	mux.HandleFunc("/api/comments/resolve", s.handleCommentsResolve)
	mux.HandleFunc("/api/comments/delete", s.handleCommentsDelete)
	mux.HandleFunc("/api/events", s.handleEvents)
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// versionPayload is the protocol handshake body: every client checks it (GET
// /api/version, and the SSE stream's first "hello" event) before trusting
// anything else the daemon says. Protocol is model.ProtocolVersion — a wt
// built from a different checkout that disagrees on it refuses to proceed.
type versionPayload struct {
	Protocol  int    `json:"protocol"`
	Version   string `json:"version"`
	GoVersion string `json:"goVersion"`
}

func currentVersion() versionPayload {
	return versionPayload{Protocol: model.ProtocolVersion, Version: version, GoVersion: runtime.Version()}
}

func (s *server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, currentVersion())
}

// rulePacksPayload is statusPayload's additive `rulePacks` field: how many
// currently-known repos are running a validly-loaded .wtcockpit.toml pack vs
// a malformed one that fell back to global rules (P5-design.md §1.3, §2).
type rulePacksPayload struct {
	Loaded int `json:"loaded"`
	Errors int `json:"errors"`
}

// statusPayload is wt status's raw payload: a human-rendered block by
// default, or exactly this shape with --json. Repo/worktree/file counts come
// straight off the existing registry snapshot (s.eng.List()) — cheap, and no
// new engine/registry accessor needed for it.
type statusPayload struct {
	Version       string           `json:"version"`
	Protocol      int              `json:"protocol"`
	UptimeSeconds float64          `json:"uptimeSeconds"`
	SocketPath    string           `json:"socketPath"`
	WatcherMode   string           `json:"watcherMode"`
	Roots         []string         `json:"roots"`
	StatePath     string           `json:"statePath"`
	WebAddr       string           `json:"webAddr"` // "" when -web is off; see wt open (P4-design.md §1.6)
	RepoCount     int              `json:"repoCount"`
	WorktreeCount int              `json:"worktreeCount"`
	ReviewedFiles int              `json:"reviewedFiles"`
	TotalFiles    int              `json:"totalFiles"`
	RulePacks     rulePacksPayload `json:"rulePacks"`
	Notifier      string           `json:"notifier"` // "osascript" | "notify-send" | "disabled (config)" | "unavailable (no notifier binary)"
	// LastRefreshMs/LastRefreshOneMs are additive (P6-design.md §6.3 layer 3,
	// the v0.2 roadmap's "scan timings" IOU): the most recently completed
	// full Refresh / targeted RefreshOne wall-clock duration, in
	// milliseconds. 0 before either has completed once.
	LastRefreshMs    float64 `json:"lastRefreshMs"`
	LastRefreshOneMs float64 `json:"lastRefreshOneMs"`
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	wts := s.eng.List()
	repos := map[string]bool{}
	var reviewed, total int
	for _, wt := range wts {
		repos[wt.Repo] = true
		reviewed += wt.Reviewed
		total += wt.Stats.Files
	}
	loaded, errs := s.eng.RulePackStats()
	// s.notifier is nil only for a *server built directly in a test without
	// going through main (see the server struct's doc comment) — every
	// production daemon always has one.
	notifierStatus := "disabled (config)"
	if s.notifier != nil {
		notifierStatus = s.notifier.Status()
	}
	full, one := s.eng.LastRefreshDurations()
	writeJSON(w, statusPayload{
		Version:          version,
		Protocol:         model.ProtocolVersion,
		UptimeSeconds:    time.Since(s.startedAt).Seconds(),
		SocketPath:       s.socketPath,
		WatcherMode:      s.watcherMode,
		Roots:            s.roots,
		StatePath:        s.statePath,
		WebAddr:          s.webAddr,
		RepoCount:        len(repos),
		WorktreeCount:    len(wts),
		ReviewedFiles:    reviewed,
		TotalFiles:       total,
		RulePacks:        rulePacksPayload{Loaded: loaded, Errors: errs},
		Notifier:         notifierStatus,
		LastRefreshMs:    durationMs(full),
		LastRefreshOneMs: durationMs(one),
	})
}

// durationMs converts a time.Duration to fractional milliseconds — plain
// d.Milliseconds() truncates to an int64, which would round every §6
// microbenchmark-scale duration (sub-millisecond to low-millisecond) down to
// 0 or a coarse integer; the perf counters this feeds are exactly the ones
// that need that precision.
func durationMs(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func (s *server) handleWorktrees(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.eng.List())
}

// handleRules answers GET /api/rules?id=<worktreeID>: the effective,
// provenance-tagged rule set for that worktree's owning repo
// (P5-design.md §1.3) — `wt rules`'s data source.
func (s *server) handleRules(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	eff, ok := s.eng.Rules(id)
	if !ok {
		http.Error(w, "unknown worktree id", http.StatusNotFound)
		return
	}
	writeJSON(w, eff)
}

func (s *server) handleDiff(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	d, ok := s.eng.Diff(id)
	if !ok {
		http.Error(w, "unknown worktree id", http.StatusNotFound)
		return
	}
	// ponytail: two separate engine reads rather than one atomic accessor —
	// a concurrent refresh landing between them can only produce a
	// momentarily-stale reviewed flag, which self-heals on the very next
	// diff.ready/review.changed event; not worth a wider lock for a
	// read-only display field.
	d.Reviewed, _ = s.eng.ReviewedMap(id)
	writeJSON(w, d)
}

func (s *server) handleReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		File     string `json:"file"`
		Reviewed bool   `json:"reviewed"`
		Hash     string `json:"hash"` // optional: the file hash the caller last viewed
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err := s.eng.SetReviewed(req.ID, req.File, req.Reviewed, req.Hash)
	switch {
	case err == nil:
		writeJSON(w, map[string]bool{"ok": true})
	case errors.Is(err, engine.ErrFileChanged):
		http.Error(w, "file changed since viewed: "+err.Error(), http.StatusConflict)
	case errors.Is(err, engine.ErrFileNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Snapshot repo/file-count before Approve runs: on success it removes the
	// worktree from the registry, so this is the last point they're available.
	// Approve is the product's only mutation — it must leave a log trail
	// regardless of whether the gates let it through.
	var repo string
	var files int
	if pre := s.eng.Registry().Get(req.ID); pre != nil {
		repo, files = pre.Repo, pre.Stats.Files
	}

	res, err := s.eng.Approve(req.ID)
	outcome := "ok"
	if err != nil {
		outcome = "denied: " + err.Error()
	}
	slog.Info("AUDIT approve", "worktree", req.ID, "repo", repo, "files", files, "outcome", outcome)

	if err != nil {
		// 409 Conflict: the request was well-formed but a gate (review/clean/merge)
		// refused it. The client prints the message verbatim.
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, res)
}

func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.Refresh(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleComments serves both verbs the REST table puts on this one path
// (P4-design.md §1.5): GET lists, POST creates. Available on every listener,
// same as every other /api/ route — the socket is tokenless for agents/CLI,
// the future web listener sits behind its own middleware (§1.3, WP2).
func (s *server) handleComments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleCommentsList(w, r)
	case http.MethodPost:
		s.handleCommentsCreate(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleCommentsList answers GET /api/comments?id=<wt>[&state=][&file=].
// state defaults to "open"; "resolved"/"all" are the only other accepted
// values. Filtering by state/file happens here, not in the engine
// (engine.Comments always returns the full set) — it's presentation, not a
// validated business rule.
func (s *server) handleCommentsList(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	views, err := s.eng.Comments(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	if state != "open" && state != "resolved" && state != "all" {
		http.Error(w, fmt.Sprintf("invalid state %q (want open, resolved, or all)", state), http.StatusBadRequest)
		return
	}
	file := r.URL.Query().Get("file")

	filtered := make([]model.CommentView, 0, len(views))
	for _, v := range views {
		if state != "all" && v.State != state {
			continue
		}
		if file != "" && v.File != file {
			continue
		}
		filtered = append(filtered, v)
	}

	// path/branch/base come from the registry — what lets an agent locate the
	// worktree and open file:line directly from the JSON alone (§1.5).
	var path, branch, base string
	if wt := s.eng.Registry().Get(id); wt != nil {
		path, branch, base = wt.Path, wt.Branch, wt.Base
	}
	writeJSON(w, model.CommentsPayload{
		WorktreeID: id,
		Path:       path,
		Branch:     branch,
		Base:       base,
		Comments:   filtered,
	})
}

// handleCommentsCreate answers POST /api/comments. The request body is
// capped at 1 MiB here — a decode-time safety net distinct from the comment
// BODY text's own 64 KiB business-rule cap, which is engine.AddComment's job
// (engine.ErrCommentTooLarge -> 413 below); see P4-design.md §1.3.
func (s *server) handleCommentsCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		ID     string `json:"id"`
		File   string `json:"file"`
		Line   int    `json:"line"`
		Side   string `json:"side"`
		Body   string `json:"body"`
		Author string `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, err := s.eng.AddComment(req.ID, req.File, req.Line, req.Side, req.Body, req.Author)
	switch {
	case err == nil:
		writeJSON(w, c)
	case errors.Is(err, engine.ErrCommentTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	case errors.Is(err, engine.ErrFileNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, engine.ErrInvalidComment):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// commentActionRequest is the shared body shape for resolve/delete: both take
// only {id, commentId}.
type commentActionRequest struct {
	ID        string `json:"id"`
	CommentID string `json:"commentId"`
}

func (s *server) handleCommentsResolve(w http.ResponseWriter, r *http.Request) {
	var req commentActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeCommentActionResult(w, s.eng.ResolveComment(req.ID, req.CommentID))
}

func (s *server) handleCommentsDelete(w http.ResponseWriter, r *http.Request) {
	var req commentActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeCommentActionResult(w, s.eng.DeleteComment(req.ID, req.CommentID))
}

// writeCommentActionResult shapes ResolveComment/DeleteComment's error into
// the REST table's status codes: unknown worktree and unknown comment id
// both 404 (the table doesn't distinguish them), anything else 500.
func writeCommentActionResult(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		writeJSON(w, map[string]bool{"ok": true})
	case errors.Is(err, engine.ErrFileNotFound), errors.Is(err, engine.ErrCommentNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleEvents streams the live event bus as Server-Sent Events. The very
// first frame is always a named "hello" event carrying the same payload as
// GET /api/version, ahead of the snapshot marker and the live stream (see
// currentVersion and model.ProtocolVersion). wt's watch parser recognises and
// skips it — it already checked the handshake via GET /api/version before
// running any command — but this keeps the SSE stream self-describing for
// any future client that only ever consumes it directly.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sendSSEEvent(w, "hello", currentVersion())
	flusher.Flush()

	ch, cancel := s.eng.Registry().Subscribe(64)
	defer cancel()

	// Prime the client with a snapshot marker so it can pull initial state.
	sendSSE(w, model.Event{Type: "snapshot", At: time.Now()})
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			sendSSE(w, e)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func sendSSE(w http.ResponseWriter, e model.Event) {
	b, _ := json.Marshal(e)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

// sendSSEEvent writes a *named* SSE event ("event: <name>" then "data: ..."),
// unlike sendSSE's unnamed data-only frames — it's how the hello handshake
// preamble distinguishes itself on the wire from an ordinary model.Event.
func sendSSEEvent(w http.ResponseWriter, name string, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
}
