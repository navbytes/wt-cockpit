// Comment operations: validation lives here (the trust boundary; handlers
// stay thin), along with the stale/orphaned computation against the
// worktree's current cached diff and the comment.changed delta emission. See
// .claude/company/handoffs/P4-design.md §1.5 for the frozen contract.
package engine

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/user"
	"time"
	"unicode/utf8"

	"github.com/navbytes/wt-cockpit/internal/diffparse"
	"github.com/navbytes/wt-cockpit/internal/model"
	"github.com/navbytes/wt-cockpit/internal/store"
)

// maxCommentBodyBytes is the frozen per-comment body size cap (P4-design.md
// §1.5/§1.3) — distinct from the handler's transport-level 1 MiB JSON decode
// cap, which guards the whole request rather than this one field.
const maxCommentBodyBytes = 64 * 1024

// maxCommentAuthorBytes caps an explicit --author/author POST field (P7
// security MEDIUM-1/LOW-2): unlike body, author previously had no size limit
// of its own at all beyond the handler's 1 MiB transport cap, letting ~500
// comments balloon a worktree's store by hundreds of MB. A smaller cap than
// body's is fine — an author is a name, not a note.
const maxCommentAuthorBytes = 256

// ErrInvalidComment wraps an AddComment validation failure (empty/invalid-
// UTF-8 body, bad side) — errors.Is unwraps to this one sentinel regardless
// of which check failed, so cmd/wtd can map every case to 400 with one
// comparison. ErrCommentTooLarge is deliberately its own sentinel below
// rather than wrapped under this one, because it alone maps to 413.
var ErrInvalidComment = errors.New("invalid comment")

// ErrCommentTooLarge is AddComment's dedicated oversized-body error (413).
var ErrCommentTooLarge = errors.New("comment body exceeds 64 KiB limit")

// ErrCommentNotFound aliases the store's sentinel so callers (cmd/wtd) only
// ever need to import engine for comment errors, mirroring how
// ErrFileNotFound/ErrFileChanged already work for review.
var ErrCommentNotFound = store.ErrCommentNotFound

// ErrTooManyComments aliases the store's per-worktree comment cap sentinel
// (413, mirroring ErrCommentTooLarge's own per-comment cap) — same rationale
// as ErrCommentNotFound above: cmd/wtd only ever imports engine for these.
var ErrTooManyComments = store.ErrTooManyComments

// ErrWorktreeNotFound is AddComment/Comments' sentinel for an unknown
// worktree id, distinct from ErrFileNotFound (a known worktree whose current
// diff just doesn't contain the named file) — P7-ux.md backlog: the two used
// to share ErrFileNotFound's "file not found in current diff" text, which is
// a confusing 404 body for a request that never even named a real worktree.
var ErrWorktreeNotFound = errors.New("unknown worktree id")

// AddComment creates a comment anchored to file within id's current diff.
// id must be a known worktree (else ErrWorktreeNotFound) and file must be
// part of its current diff (else ErrFileNotFound). line 0 is the file-level
// convention; negative lines are rejected. side defaults to "new" when empty
// and is otherwise restricted to "old"/"new". body must be non-empty, valid
// UTF-8, and at most maxCommentBodyBytes; author, when given explicitly,
// must be valid UTF-8 and at most maxCommentAuthorBytes, else it defaults to
// the daemon's OS user. Both body and author are run through
// diffparse.SanitizeControl before storing (P7 security MEDIUM-1/LOW-2): this
// is the one ingest chokepoint every comment — CLI, curl, or the web form —
// passes through, so a raw ESC/OSC byte can never persist into SQLite and
// later print raw from `wt comments`. The comment is stamped with a fresh
// ID, At and the file's current hash (its stale-anchor baseline), persisted,
// and published as comment.changed.
func (e *Engine) AddComment(id, file string, line int, side, body, author string) (model.Comment, error) {
	e.mu.RLock()
	m, ok := e.cache[id]
	e.mu.RUnlock()
	if !ok {
		return model.Comment{}, ErrWorktreeNotFound
	}
	cur := findDiffFile(m.diff.Files, file)
	if cur == nil {
		return model.Comment{}, ErrFileNotFound
	}
	if line < 0 {
		return model.Comment{}, fmt.Errorf("%w: line must be >= 0 (0 = file-level)", ErrInvalidComment)
	}
	switch side {
	case "":
		side = "new"
	case "old", "new":
	default:
		return model.Comment{}, fmt.Errorf("%w: side must be \"old\" or \"new\", got %q", ErrInvalidComment, side)
	}
	if body == "" {
		return model.Comment{}, fmt.Errorf("%w: body must not be empty", ErrInvalidComment)
	}
	if !utf8.ValidString(body) {
		return model.Comment{}, fmt.Errorf("%w: body is not valid UTF-8", ErrInvalidComment)
	}
	if len(body) > maxCommentBodyBytes {
		return model.Comment{}, ErrCommentTooLarge
	}
	if author == "" {
		author = defaultAuthor()
	} else {
		if !utf8.ValidString(author) {
			return model.Comment{}, fmt.Errorf("%w: author is not valid UTF-8", ErrInvalidComment)
		}
		if len(author) > maxCommentAuthorBytes {
			return model.Comment{}, fmt.Errorf("%w: author exceeds %d byte limit", ErrInvalidComment, maxCommentAuthorBytes)
		}
	}

	c := model.Comment{
		ID:         newCommentID(),
		WorktreeID: id,
		File:       file,
		Line:       line,
		Side:       side,
		Body:       diffparse.SanitizeControl(body),
		Author:     diffparse.SanitizeControl(author),
		State:      "open",
		FileHash:   cur.Hash,
		At:         time.Now(),
	}
	if err := e.st.AddComment(c); err != nil {
		return model.Comment{}, err
	}
	e.reg.Publish(model.Event{Type: model.EventCommentChanged, ID: id, At: c.At})
	return c, nil
}

// Comments returns every stored comment for id, enriched with Stale/Orphaned
// computed against the worktree's *current* cached diff — never persisted,
// always derived fresh at read time (P4-design.md §1.5's frozen semantics).
// Stale means the file is still in the diff but its hash moved since the
// comment was made; Orphaned means the file left the diff entirely. id must
// be a known worktree, else ErrWorktreeNotFound.
func (e *Engine) Comments(id string) ([]model.CommentView, error) {
	e.mu.RLock()
	m, ok := e.cache[id]
	e.mu.RUnlock()
	if !ok {
		return nil, ErrWorktreeNotFound
	}
	raw, err := e.st.Comments(id)
	if err != nil {
		return nil, err
	}

	hashes := make(map[string]string, len(m.diff.Files))
	for _, f := range m.diff.Files {
		hashes[f.Path] = f.Hash
	}
	views := make([]model.CommentView, 0, len(raw))
	for _, c := range raw {
		cur, present := hashes[c.File]
		views = append(views, model.CommentView{
			Comment:  c,
			Stale:    present && cur != c.FileHash,
			Orphaned: !present,
		})
	}
	return views, nil
}

// ResolveComment marks a comment resolved and publishes comment.changed.
func (e *Engine) ResolveComment(id, commentID string) error {
	if !e.knownWorktree(id) {
		return ErrFileNotFound
	}
	if err := e.st.ResolveComment(id, commentID); err != nil {
		return err
	}
	e.reg.Publish(model.Event{Type: model.EventCommentChanged, ID: id, At: time.Now()})
	return nil
}

// DeleteComment removes a comment outright and publishes comment.changed.
func (e *Engine) DeleteComment(id, commentID string) error {
	if !e.knownWorktree(id) {
		return ErrFileNotFound
	}
	if err := e.st.DeleteComment(id, commentID); err != nil {
		return err
	}
	e.reg.Publish(model.Event{Type: model.EventCommentChanged, ID: id, At: time.Now()})
	return nil
}

func (e *Engine) knownWorktree(id string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, ok := e.cache[id]
	return ok
}

// newCommentID mints a "c-" prefixed 16-hex-char identifier (8 random
// bytes), engine-assigned per P4-design.md §1.5.
func newCommentID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "c-" + hex.EncodeToString(b)
}

// defaultAuthor is the daemon-side attribution fallback when a comment POST
// omits one: the OS user running wtd. Single-user tool, no multi-user
// identity system — see P4-design.md §1.5.
func defaultAuthor() string {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return "unknown"
	}
	return u.Username
}
