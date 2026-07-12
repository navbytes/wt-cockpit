// Command wtd is the cockpit daemon. It runs the engine and exposes it over an
// HTTP API served on a Unix socket (and optionally TCP for remote/Tailscale use).
// The same handler powers the TUI client, the web reading room, and a future
// menu-bar app — every frontend is a thin client of this one process.
package main

import (
	"context"
	"encoding/json"
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
	interval := flag.Duration("interval", 2*time.Second, "poll interval")
	flag.Parse()

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
	gr := guardrail.New(guardrail.DefaultRules())
	eng := engine.New(engine.Config{
		Roots:          roots,
		DefaultBase:    *base,
		ActivityWindow: 30 * time.Second,
	}, gitbackend.NewCLI(), reg, st, gr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Watcher drives refreshes; swap the Poller for an fsnotify watcher later.
	go (&watcher.Poller{Interval: *interval}).Run(ctx, func(string) {
		if err := eng.Refresh(ctx); err != nil && ctx.Err() == nil {
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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.eng.SetReviewed(req.ID, req.File, req.Reviewed); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
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
