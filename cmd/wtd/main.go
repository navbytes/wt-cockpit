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
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/navbytes/wt-cockpit/internal/config"
	"github.com/navbytes/wt-cockpit/internal/engine"
	"github.com/navbytes/wt-cockpit/internal/gitbackend"
	"github.com/navbytes/wt-cockpit/internal/guardrail"
	"github.com/navbytes/wt-cockpit/internal/model"
	"github.com/navbytes/wt-cockpit/internal/registry"
	"github.com/navbytes/wt-cockpit/internal/store"
	"github.com/navbytes/wt-cockpit/internal/watcher"
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

func main() {
	var roots multiFlag
	home, _ := os.UserHomeDir()
	dataDir := filepath.Join(home, ".wtcockpit")

	flag.Var(&roots, "root", "root directory to scan for repos (repeatable, comma-ok)")
	socket := flag.String("socket", filepath.Join(dataDir, "wtd.sock"), "unix socket path")
	tcp := flag.String("tcp", "", "optional TCP address to also listen on (e.g. 127.0.0.1:7799)")
	statePath := flag.String("state", filepath.Join(dataDir, "state.json"), "review state file")
	base := flag.String("base", "", "diff baseline branch (default: each repo's own default)")
	interval := flag.Duration("interval", 2*time.Second, "poll interval (also governs the fsnotify reconciliation tick)")
	watchMode := flag.String("watch", "fsnotify", "watcher backend: fsnotify (default) or poll")
	configPath := flag.String("config", config.DefaultPath(), "path to config.toml (roots, base, guardrails, daemon options)")
	flag.Parse()

	// Precedence is explicit flags > config file > the built-in defaults set
	// above. flag.Visit only calls back for flags the user actually passed, so
	// it's how we tell "explicit -base main" apart from "-base defaulted to ''".
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	// -config was explicitly requested: unlike the conventional default path, a
	// missing or invalid file here is fatal — the user asked for this file.
	if explicit["config"] {
		if _, statErr := os.Stat(*configPath); statErr != nil {
			log.Fatalf("-config %s: %v", *configPath, statErr)
		}
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config %s: %v", *configPath, err)
	}

	roots = mergeRoots(roots, explicit["root"], cfg.Roots)
	*base = mergeSetting(*base, explicit["base"], cfg.Base)
	*socket = mergeSetting(*socket, explicit["socket"], cfg.Socket)
	*tcp = mergeSetting(*tcp, explicit["tcp"], cfg.TCP)
	*statePath = mergeSetting(*statePath, explicit["state"], cfg.State)
	*interval = mergeSetting(*interval, explicit["interval"], cfg.Interval)
	*watchMode = mergeSetting(*watchMode, explicit["watch"], cfg.Watch)

	if len(roots) == 0 {
		if cwd, err := os.Getwd(); err == nil {
			roots = multiFlag{cwd}
		}
	}
	_ = os.MkdirAll(dataDir, 0o755)

	st, err := store.OpenJSON(*statePath)
	if err != nil {
		log.Fatalf("open state: %v", err)
	}
	reg := registry.New()
	gr := guardrail.New(cfg.RulesOr(guardrail.DefaultRules()))
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
		log.Fatalf("unknown -watch value %q (want fsnotify or poll)", *watchMode)
	}
	go wch.Run(ctx, func(path string) {
		var err error
		if path == "" {
			err = eng.Refresh(ctx)
		} else {
			err = eng.RefreshOne(ctx, path)
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("refresh: %v", err)
		}
	})

	srv := &server{eng: eng}
	handler := srv.routes()

	// Listen on the unix socket (primary transport).
	_ = os.Remove(*socket)
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		log.Fatalf("listen unix %s: %v", *socket, err)
	}
	defer os.Remove(*socket)
	log.Printf("wtd listening on %s (roots: %s)", *socket, roots.String())

	httpSrv := &http.Server{Handler: handler}
	go func() { _ = httpSrv.Serve(ln) }()

	if *tcp != "" {
		tln, err := net.Listen("tcp", *tcp)
		if err != nil {
			log.Fatalf("listen tcp %s: %v", *tcp, err)
		}
		log.Printf("wtd also listening on http://%s", *tcp)
		go func() { _ = httpSrv.Serve(tln) }()
	}

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	log.Print("wtd stopped")
}

// mergeSetting resolves one setting's final value under flags > config >
// built-in-default precedence. flagVal is the flag variable's value after
// flag.Parse: the user's explicit value if explicit is true, otherwise
// whatever built-in default it was registered with. cfgVal is the config
// file's value for the same setting (""/zero means the config didn't set it).
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
func baseFor(cfg config.Config) map[string]string {
	if len(cfg.Repos) == 0 {
		return nil
	}
	m := make(map[string]string, len(cfg.Repos))
	for path, rc := range cfg.Repos {
		if rc.Base != "" {
			m[filepath.Clean(path)] = rc.Base
		}
	}
	return m
}

type server struct{ eng *engine.Engine }

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/api/worktrees", s.handleWorktrees)
	mux.HandleFunc("/api/diff", s.handleDiff)
	mux.HandleFunc("/api/review", s.handleReview)
	mux.HandleFunc("/api/approve", s.handleApprove)
	mux.HandleFunc("/api/refresh", s.handleRefresh)
	mux.HandleFunc("/api/events", s.handleEvents)
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) handleWorktrees(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.eng.List())
}

func (s *server) handleDiff(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	d, ok := s.eng.Diff(id)
	if !ok {
		http.Error(w, "unknown worktree id", http.StatusNotFound)
		return
	}
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
	res, err := s.eng.Approve(req.ID)
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

// handleEvents streams the live event bus as Server-Sent Events.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

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
