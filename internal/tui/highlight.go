package tui

import (
	"container/list"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	chromastyles "github.com/alecthomas/chroma/v2/styles"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// chromaMaxLines and chromaMaxBytes are the plain-text fallback threshold
// (P3-design.md §2.5): skip chroma entirely past this size, permanently (no
// async round trip even attempted) — rendered plain with a dim note in the
// file card header (diffview.go's renderFileHeader).
const (
	chromaMaxLines = 2000
	chromaMaxBytes = 256 * 1024
)

// chromaTooLarge is computed once per file when a diff loads (diffview.go's
// reflow), not per frame — scanning every hunk line on every keystroke would
// itself violate the "no full-buffer re-style" budget it exists to protect.
func chromaTooLarge(f model.DiffFile) bool {
	bytes := 0
	for _, h := range f.Hunks {
		for _, ln := range h.Lines {
			bytes += len(ln.Content) + 1
		}
	}
	return totalHunkLines(f) > chromaMaxLines || bytes > chromaMaxBytes
}

// chromaStyle is the mock-matching palette (P3-design.md §2.5: "style
// github-dark, closest to the mock palette").
const chromaStyle = "github-dark"

// chromaFormatter maps the active lipgloss color profile to the matching
// chroma TTY formatter so both systems degrade together (§2.5/§2.6). A
// profile with no color at all (NO_COLOR / ascii, the profile tests force)
// has no ANSI formatter to degrade to — ok=false means "don't highlight",
// not an error: rows already render plain in that case.
func chromaFormatter(p termenv.Profile) (chroma.Formatter, bool) {
	switch p {
	case termenv.TrueColor:
		return formatters.TTY16m, true
	case termenv.ANSI256:
		return formatters.TTY256, true
	case termenv.ANSI:
		return formatters.TTY16, true
	default:
		return nil, false
	}
}

// concatFileContent rebuilds one file's source text from its diff hunks in
// order (P3-design.md §2.5: "chroma over that file's concatenated hunk
// lines — whole-file tokenization keeps multi-line strings/comments
// correct"). Each git diff line is already exactly one physical source
// line, so re-joining with "\n" reconstructs real multi-line syntax
// (verified against chroma v2.27.0: every per-line chunk of its TTY
// formatter output is self-contained, resetting before each newline, so
// splitting this back out by "\n" is safe even mid-token).
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

// highlightCmd tokenizes+formats one file off the Update goroutine (§2.5:
// "nothing ever blocks Update/View"). A nil Msg (no highlighting happened,
// for any reason) is a legal, ignored bubbletea result — the row already has
// its plain-text fallback.
func highlightCmd(hash, src string, lexer chroma.Lexer) tea.Cmd {
	return func() tea.Msg {
		formatter, ok := chromaFormatter(lipgloss.ColorProfile())
		if !ok {
			return nil
		}
		it, err := chroma.Coalesce(lexer).Tokenise(nil, src)
		if err != nil {
			return nil
		}
		var buf strings.Builder
		if err := formatter.Format(&buf, chromastyles.Get(chromaStyle), it); err != nil {
			return nil
		}
		split := strings.Split(buf.String(), "\n")
		if n := len(split); n > 0 && split[n-1] == "" {
			split = split[:n-1] // concatFileContent always ends with "\n"
		}
		return highlightedMsg{FileHash: hash, Lines: split}
	}
}

// visibleFileIndices returns the distinct file indices touched by
// lines[offset : offset+height), in first-seen order.
func visibleFileIndices(lines []renderLine, offset, height int) []int {
	if offset < 0 {
		offset = 0
	}
	end := offset + height
	if end > len(lines) {
		end = len(lines)
	}
	seen := map[int]bool{}
	var out []int
	for i := offset; i < end; i++ {
		fi := lines[i].fileIdx
		if !seen[fi] {
			seen[fi] = true
			out = append(out, fi)
		}
	}
	return out
}

// ensureHighlightCmds dispatches one highlightCmd per newly-visible, eligible
// file (P3-design.md §2.5: "a file card first enters the window"). Eligible
// = not binary, under the size threshold, and chroma has a lexer for its
// path — files that fail any of those checks never get an async round trip
// at all, they simply render plain forever. pending dedupes concurrent
// requests for the same file hash; it is only ever touched from the
// Update()/View() goroutine (bubbletea's single event loop), so — unlike
// highlightCache, which is genuinely shared/async-touched — it needs no
// mutex of its own.
func ensureHighlightCmds(v *diffview, hl *highlightCache, pending map[string]bool) tea.Cmd {
	var cmds []tea.Cmd
	for _, fi := range visibleFileIndices(v.lines, v.offset, v.height) {
		f := v.diff.Files[fi]
		if f.Binary || v.tooLarge[fi] || pending[f.Hash] {
			continue
		}
		if _, cached := hl.get(f.Hash); cached {
			continue
		}
		lexer := lexers.Match(f.Path)
		if lexer == nil {
			continue
		}
		pending[f.Hash] = true
		cmds = append(cmds, highlightCmd(f.Hash, concatFileContent(f), lexer))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// highlightCache is a byte-accounted LRU of one worktree-independent, file
// content-hash-keyed chroma result (P3-design.md §2.5: "16 MB cap"). It is
// shared, pointer-held state, reachable from multiple in-flight highlightCmd
// completions concurrently — hence the mutex, even though every *current*
// call site happens to serialize through bubbletea's own event loop; the
// cache is exactly the kind of long-lived shared object a future caller
// could reasonably touch from elsewhere.
type highlightCache struct {
	mu       sync.Mutex
	ll       *list.List
	index    map[string]*list.Element
	bytes    int
	maxBytes int
}

type highlightEntry struct {
	hash  string
	lines []string
	bytes int
}

const highlightCacheMaxBytes = 16 * 1024 * 1024

func newHighlightCache() *highlightCache {
	return &highlightCache{ll: list.New(), index: map[string]*list.Element{}, maxBytes: highlightCacheMaxBytes}
}

func (c *highlightCache) get(hash string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[hash]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*highlightEntry).lines, true
}

func (c *highlightCache) put(hash string, lines []string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	size := 0
	for _, l := range lines {
		size += len(l)
	}

	if el, ok := c.index[hash]; ok {
		c.bytes += size - el.Value.(*highlightEntry).bytes
		el.Value = &highlightEntry{hash: hash, lines: lines, bytes: size}
		c.ll.MoveToFront(el)
	} else {
		el := c.ll.PushFront(&highlightEntry{hash: hash, lines: lines, bytes: size})
		c.index[hash] = el
		c.bytes += size
	}

	for c.bytes > c.maxBytes {
		back := c.ll.Back()
		if back == nil {
			break
		}
		e := back.Value.(*highlightEntry)
		c.bytes -= e.bytes
		delete(c.index, e.hash)
		c.ll.Remove(back)
	}
}
