// highlight.go renders one file's diff lines to HTML for the reading room
// (P4-design.md §1.4): chroma's classes-based HTML formatter (CSP-clean — no
// inline styles), a byte-accounted per-file-hash LRU (mirrors the TUI's
// cache, P3-design.md §2.5), and the size thresholds that skip highlighting
// for large files, all sized identically to the TUI's own constants.
//
// This file (with sxs.go) is the ONLY place internal/web ever constructs
// template.HTML: chromaHTMLLines is the chroma-classes producer, plainHTMLLines
// is the escaped plain-text fallback. Every other renderer in this package
// only ever places these pre-rendered values, or emits plain strings through
// html/template's own contextual auto-escaping.
package web

import (
	"container/list"
	"html"
	"html/template"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	chromastyles "github.com/alecthomas/chroma/v2/styles"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// highlightMaxLines and highlightMaxBytes are the plain-text fallback
// threshold, mirroring the TUI's identical chromaMaxLines/chromaMaxBytes
// (P3-design.md §2.5, restated for the web listener at P4-design.md §1.4):
// past this size, a file always renders as escaped plain text with a dim
// "highlighting off (large file)" note, never fed to chroma at all.
const (
	highlightMaxLines = 2000
	highlightMaxBytes = 256 * 1024
)

// collapseAddDelThreshold and collapseGlobs are the display-only collapse
// heuristic mirrored from the TUI (P3-design.md §2.5, restated at
// P4-design.md §1.4 "same constants") — not git logic, purely how much of a
// large/generated file the room shows by default.
const collapseAddDelThreshold = 400

var collapseGlobs = []string{"go.sum", "package-lock.json", "*.lock", "yarn.lock", "*.snap", "Cargo.lock"}

// isCollapsedByDefault reports whether f collapses to a one-line summary
// card unless the caller requested it expanded (?expand=<path>, pages.go).
func isCollapsedByDefault(f model.DiffFile) bool {
	if f.Stats.Add+f.Stats.Del > collapseAddDelThreshold {
		return true
	}
	base := path.Base(f.Path)
	for _, g := range collapseGlobs {
		if ok, _ := path.Match(g, base); ok {
			return true
		}
	}
	return false
}

// totalHunkLines sums every Line across every hunk in f.
func totalHunkLines(f model.DiffFile) int {
	n := 0
	for _, h := range f.Hunks {
		n += len(h.Lines)
	}
	return n
}

// hunkBytes sums the content length (plus a separator byte per line) across
// every hunk in f — the byte half of the highlight-skip threshold.
func hunkBytes(f model.DiffFile) int {
	n := 0
	for _, h := range f.Hunks {
		for _, ln := range h.Lines {
			n += len(ln.Content) + 1
		}
	}
	return n
}

func hunkTooLarge(f model.DiffFile) bool {
	return totalHunkLines(f) > highlightMaxLines || hunkBytes(f) > highlightMaxBytes
}

// concatFileContent rebuilds one file's source text from its diff hunks in
// order, so chroma tokenizes the whole file once — multi-line constructs
// (block comments, multi-line strings) stay correct across hunk boundaries.
// Duplicated from (rather than importing) internal/tui/highlight.go's
// identical helper: web must not depend on the TUI's presentation package
// (P4-design.md §3's dependency direction is web -> engine -> store/model).
func concatFileContent(f model.DiffFile) string {
	var b strings.Builder
	first := true
	for _, h := range f.Hunks {
		for _, ln := range h.Lines {
			if !first {
				b.WriteByte('\n')
			}
			first = false
			b.WriteString(ln.Content)
		}
	}
	b.WriteByte('\n')
	return b.String()
}

// chromaStyleName matches the TUI's chosen palette (P3/P4 design: "style
// github-dark, closest to the mock palette").
const chromaStyleName = "github-dark"

var (
	// chromaHTMLFormatter is shared package-wide: WithClasses(true) emits
	// class="..." attributes instead of inline styles (P4-design.md §1.3 —
	// CSP's script-src/style-src stay 'self' with no per-element styling to
	// smuggle a payload through), PreventSurroundingPre(true) means each
	// per-line Format call below emits bare token spans with no wrapping
	// <pre>/<span class="line"> — exactly what a single srow's own
	// <span class="code chroma"> needs to wrap.
	chromaHTMLFormatter = chromahtml.New(chromahtml.WithClasses(true), chromahtml.PreventSurroundingPre(true))
	chromaStyleValue    = chromastyles.Get(chromaStyleName)
)

// chromaCSS is generated once (WriteCSS, into memory) and served at
// GET /static/chroma.css (P4-design.md §1.4/§3) — the token color rules a
// "chroma"-classed ancestor needs for WithClasses(true) output to render in
// color at all. Memoized: every app instance in a process shares the one
// (deterministic, style-name-keyed) stylesheet.
var chromaCSS = sync.OnceValue(func() []byte {
	var b strings.Builder
	_ = chromaHTMLFormatter.WriteCSS(&b, chromaStyleValue)
	return []byte(b.String())
})

func handleChromaCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write(chromaCSS())
}

// chromaHTMLLines is producer #1 of this package's two sanctioned
// template.HTML producers (P4-design.md §1.2's frozen rule): chroma's own
// html.EscapeString-per-token HTML output (verified against chroma v2.27.0),
// one template.HTML per source line, via chroma.SplitTokensIntoLines — which
// splits at the *token* level before any HTML is written, so a line's HTML
// is never produced by slicing an already-escaped string (which could cut a
// multi-byte entity or an open tag in half).
func chromaHTMLLines(lexer chroma.Lexer, src string) ([]template.HTML, bool) {
	tokens, err := chroma.Tokenise(chroma.Coalesce(lexer), nil, src)
	if err != nil {
		return nil, false
	}
	lineGroups := chroma.SplitTokensIntoLines(tokens)
	out := make([]template.HTML, len(lineGroups))
	for i, toks := range lineGroups {
		trimTrailingNewline(toks)
		var buf strings.Builder
		if err := chromaHTMLFormatter.Format(&buf, chromaStyleValue, chroma.Literator(toks...)); err != nil {
			return nil, false
		}
		out[i] = template.HTML(buf.String())
	}
	return out, true
}

// trimTrailingNewline strips the "\n" that chroma.SplitTokensIntoLines
// leaves attached to a line-group's last token (it splits at newlines but
// keeps each one attached to the line it terminates, since that's what makes
// the split lossless). Left alone, that literal "\n" would render as a hard
// line break inside the row's white-space:pre code cell instead of just
// ending the token slice. Safe to mutate in place: each output line from
// SplitTokensIntoLines owns its own freshly-appended backing slice, never
// shared with another line.
func trimTrailingNewline(toks []chroma.Token) {
	if n := len(toks); n > 0 {
		toks[n-1].Value = strings.TrimSuffix(toks[n-1].Value, "\n")
	}
}

// plainHTMLLines is producer #2 of this package's two sanctioned
// template.HTML producers: the escaped plain-text fallback for a file
// chroma didn't highlight (too large, no matching lexer, or a
// tokenize/format error) — one escaped line per hunk line, in file order.
func plainHTMLLines(f model.DiffFile) []template.HTML {
	out := make([]template.HTML, 0, totalHunkLines(f))
	for _, h := range f.Hunks {
		for _, ln := range h.Lines {
			out = append(out, template.HTML(html.EscapeString(ln.Content)))
		}
	}
	return out
}

// linesForFile returns f's per-line HTML (1:1 with its concatenated hunk
// lines, matching internal/tui/flatten.go's codeIdx convention) plus whether
// highlighting was skipped for size ("off"), consulting/populating a's
// highlight cache first. Binary files have nothing to highlight — pages.go
// never calls this for them.
func (a *app) linesForFile(f model.DiffFile) (lines []template.HTML, off bool) {
	if cached, ok := a.hl.get(f.Hash); ok {
		return cached.lines, cached.off
	}

	off = hunkTooLarge(f)
	if !off {
		if lexer := lexers.Match(f.Path); lexer != nil {
			if got, ok := chromaHTMLLines(lexer, concatFileContent(f)); ok {
				lines = got
			}
		}
	}
	if lines == nil {
		lines = plainHTMLLines(f)
	}

	a.hl.put(f.Hash, lines, off)
	return lines, off
}

// highlightCacheMaxBytes is the "16 MiB cap" (P4-design.md §1.4/§6) — this
// cache lives in wtd, a separate process from the TUI's own identically-
// sized cache, so the two budgets are independent, not shared or halved.
const highlightCacheMaxBytes = 16 * 1024 * 1024

// lineCacheEntry is one highlightCache entry: hash is kept alongside the
// value (not just as the index map's key) so eviction can remove the right
// map entry once an element falls off the LRU list's back.
type lineCacheEntry struct {
	hash  string
	lines []template.HTML
	off   bool
	bytes int
}

// highlightCache is a byte-accounted LRU of one DiffFile.Hash-keyed render
// result (content-addressed: survives a commit inside the worktree, since
// that never changes a file's own content, and auto-invalidates the instant
// it does). Mirrors internal/tui/highlight.go's highlightCache shape;
// duplicated rather than shared for the same reason as concatFileContent —
// this package must not import the TUI's.
type highlightCache struct {
	mu       sync.Mutex
	ll       *list.List
	index    map[string]*list.Element
	bytes    int
	maxBytes int
}

func newHighlightCache() *highlightCache {
	return &highlightCache{ll: list.New(), index: map[string]*list.Element{}, maxBytes: highlightCacheMaxBytes}
}

func (c *highlightCache) get(hash string) (lineCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[hash]
	if !ok {
		return lineCacheEntry{}, false
	}
	c.ll.MoveToFront(el)
	return *el.Value.(*lineCacheEntry), true
}

func (c *highlightCache) put(hash string, lines []template.HTML, off bool) {
	size := 0
	for _, l := range lines {
		size += len(l)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[hash]; ok {
		old := el.Value.(*lineCacheEntry)
		c.bytes += size - old.bytes
		el.Value = &lineCacheEntry{hash: hash, lines: lines, off: off, bytes: size}
		c.ll.MoveToFront(el)
	} else {
		el := c.ll.PushFront(&lineCacheEntry{hash: hash, lines: lines, off: off, bytes: size})
		c.index[hash] = el
		c.bytes += size
	}

	for c.bytes > c.maxBytes {
		back := c.ll.Back()
		if back == nil {
			break
		}
		e := back.Value.(*lineCacheEntry)
		c.bytes -= e.bytes
		delete(c.index, e.hash)
		c.ll.Remove(back)
	}
}
