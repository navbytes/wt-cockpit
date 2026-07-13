// Package web is wtd's opt-in browser-facing listener: server-rendered pages
// (html/template, go:embed — no Node toolchain, no client-side framework) and
// the browser-security middleware stack that guards them (P4-design.md §1.1,
// §1.3). It sits in front of the daemon's existing API mux — the exact same
// http.Handler value the Unix socket and -tcp already serve — so approve and
// every other route work identically over the web listener; nothing here
// duplicates engine logic. Dependency direction: web → engine → store/model;
// the engine stays HTTP/UI-free.
package web

import (
	"embed"
	"html/template"
	"net/http"

	"github.com/navbytes/wt-cockpit/internal/engine"
)

//go:embed static
var staticFS embed.FS

//go:embed templates/layout.tmpl templates/index.tmpl templates/room.tmpl templates/fragments.tmpl
var templateFS embed.FS

// Config carries what New needs beyond the engine and the shared API
// handler: the address the web listener is actually bound to (Host/Origin
// allowlists are keyed off it — correct even under an ephemeral "127.0.0.1:0"
// bind, since the caller resolves the real bound address first) and the
// process-lifetime CSRF token every rendered page embeds and every
// state-changing request must echo back (§1.3).
type Config struct {
	BoundAddr string
	CSRFToken string
	Roots     []string // configured scan roots, for the index page's empty-state message
}

// app holds what every handler needs. Unexported: cmd/wtd only ever sees the
// http.Handler New returns.
type app struct {
	eng  *engine.Engine
	cfg  Config
	tmpl map[string]*template.Template
	hl   *highlightCache
}

// New builds the web listener's handler (P4-design.md §1.1/§3): the index
// page and its live fragment, the reading room, embedded static assets and
// the generated chroma stylesheet, and the daemon's existing API mux (api)
// mounted at /api/ — all behind the browser-security middleware stack
// (secure, in middleware.go).
func New(eng *engine.Engine, api http.Handler, cfg Config) http.Handler {
	a := &app{eng: eng, cfg: cfg, tmpl: parseTemplates(), hl: newHighlightCache()}
	_ = chromaCSS() // computed once at startup (P4-design.md §1.4), not on first request

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /wt/{id}", a.handleRoom)
	mux.HandleFunc("GET /fragment/worktrees", a.handleFragmentWorktrees)
	mux.HandleFunc("GET /fragment/rail", a.handleFragmentRail)
	mux.HandleFunc("GET /fragment/comments", a.handleFragmentComments)
	mux.HandleFunc("GET /static/chroma.css", handleChromaCSS) // more specific than /static/, wins regardless of registration order
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.Handle("/api/", api) // same handler value the socket/-tcp serve; no route changes

	return secure(mux, cfg)
}

// parseTemplates builds one *template.Template per page, each its own clone
// of the shared layout so "content" (and any other per-page name) can be
// defined independently without colliding across pages in a single global
// template namespace — the standard html/template layout-inheritance
// pattern.
func parseTemplates() map[string]*template.Template {
	layout := template.Must(template.New("layout").Funcs(templateFuncs).ParseFS(templateFS, "templates/layout.tmpl"))

	index := template.Must(layout.Clone())
	index = template.Must(index.ParseFS(templateFS, "templates/index.tmpl"))

	// room additionally parses fragments.tmpl: room.tmpl's per-file loop calls
	// {{template "fileCommentsStrip"}}/"composerTemplate" defined there, and
	// the comments/rail fragment handlers execute named blocks ("rail",
	// "commentBlocks") out of this same parsed template — one definition of
	// each block, so the full page and its live-refresh fragments can never
	// render it differently.
	room := template.Must(layout.Clone())
	room = template.Must(room.ParseFS(templateFS, "templates/room.tmpl", "templates/fragments.tmpl"))

	return map[string]*template.Template{"index": index, "room": room}
}
