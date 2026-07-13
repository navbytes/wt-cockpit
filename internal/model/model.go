// Package model holds the core domain types shared across the engine and clients.
// These types are transport-agnostic: they are what the daemon serialises to JSON
// and what every frontend (TUI, web, menu-bar) consumes.
package model

import "time"

// ProtocolVersion is the daemon↔client wire protocol version, reported by
// wtd at GET /api/version and as the SSE stream's first "hello" event. A
// client refuses to proceed against a daemon reporting a different value
// rather than risk silently misinterpreting a shape it doesn't understand.
const ProtocolVersion = 1

// AgentKind identifies which tool is (likely) driving a worktree. It is inferred,
// never authoritative — the cockpit is agent-agnostic and only reads git/fs.
type AgentKind string

const (
	AgentUnknown AgentKind = "unknown"
	AgentClaude  AgentKind = "claude"
	AgentCodex   AgentKind = "codex"
	AgentAider   AgentKind = "aider"
	AgentGit     AgentKind = "git"
)

// WorktreeState is the at-a-glance status used to sort and colour the radar.
type WorktreeState string

const (
	StateIdle   WorktreeState = "idle"   // clean-ish, nothing happening recently
	StateActive WorktreeState = "active" // changed within the activity window
	StateDirty  WorktreeState = "dirty"  // uncommitted changes, not recently touched
)

// Repo is a discovered git repository (its main working tree).
type Repo struct {
	Name string `json:"name"`
	Path string `json:"path"` // absolute path to the repo root
	Lang string `json:"lang"` // best-effort language tag for display
}

// Stats is the churn summary for a worktree or a file.
type Stats struct {
	Files int `json:"files"`
	Add   int `json:"add"`
	Del   int `json:"del"`
}

// Worktree is one git worktree belonging to a Repo. It is the primary unit the
// cockpit tracks. ID is stable for the lifetime of the worktree (derived from path).
type Worktree struct {
	ID         string         `json:"id"`
	Repo       string         `json:"repo"`   // owning repo name
	Name       string         `json:"name"`   // worktree/branch short name
	Path       string         `json:"path"`   // absolute path
	Branch     string         `json:"branch"` // checked-out branch
	Base       string         `json:"base"`   // diff baseline (e.g. "main")
	Agent      AgentKind      `json:"agent"`
	State      WorktreeState  `json:"state"`
	Stats      Stats          `json:"stats"`
	LastChange time.Time      `json:"lastChange"`
	DiffHash   string         `json:"diffHash"`   // changes when the diff content changes
	Guardrails []GuardrailHit `json:"guardrails"` // rules currently tripped
	Reviewed   int            `json:"reviewed"`   // count of files marked reviewed
}

// LineKind classifies a rendered diff line.
type LineKind string

const (
	LineContext LineKind = "ctx"
	LineAdd     LineKind = "add"
	LineDel     LineKind = "del"
)

// Line is a single line within a hunk, carrying both old and new line numbers
// (either may be 0 when not applicable) so a client can render unified or split.
type Line struct {
	Kind    LineKind `json:"kind"`
	OldNum  int      `json:"oldNum"`
	NewNum  int      `json:"newNum"`
	Content string   `json:"content"`
}

// Hunk is a contiguous @@ ... @@ block.
type Hunk struct {
	Header   string `json:"header"`
	OldStart int    `json:"oldStart"`
	NewStart int    `json:"newStart"`
	Lines    []Line `json:"lines"`
}

// FileStatus mirrors git's status letters we care about.
type FileStatus string

const (
	FileModified FileStatus = "modified"
	FileAdded    FileStatus = "added"
	FileDeleted  FileStatus = "deleted"
	FileRenamed  FileStatus = "renamed"
)

// DiffFile is one file's worth of change within a worktree diff.
type DiffFile struct {
	Path    string     `json:"path"`    // new path (or old path for deletes)
	OldPath string     `json:"oldPath"` // set on rename
	Status  FileStatus `json:"status"`
	Stats   Stats      `json:"stats"`
	Hunks   []Hunk     `json:"hunks"`
	Binary  bool       `json:"binary"`
	// OldBlob/NewBlob are the git object ids from the diff's "index a..b"
	// header line, when git emitted one (it's omitted for a pure,
	// content-identical rename; either side is the all-zero placeholder for
	// /dev/null on an add/delete). They let Hash notice a content change even
	// when there are zero hunks — notably binary files, which git never
	// renders as hunks — since the blob id always moves when the bytes do.
	OldBlob string `json:"oldBlob,omitempty"`
	NewBlob string `json:"newBlob,omitempty"`
	// Hash is a stable per-file identity: sha1(path + status + blobs + every
	// hunk line). Unlike the whole-diff hash it survives a commit inside the
	// worktree (the content doesn't change), which is what lets review state
	// be keyed per file instead of resetting whenever anything in the
	// worktree changes.
	Hash string `json:"hash"`
}

// Diff is the full structured diff for a worktree.
type Diff struct {
	WorktreeID string     `json:"worktreeId"`
	Base       string     `json:"base"`
	Hash       string     `json:"hash"`
	Files      []DiffFile `json:"files"`
	// Reviewed maps each file's path to whether it is currently marked
	// reviewed (WP3 addition: cmd/wtd's handleDiff computes this from the
	// engine's existing per-file hash matching, engine.Engine.ReviewedMap).
	// Additive JSON (omitempty) — a pre-WP3 client simply never sees the
	// field, per docs/02-stack-decision.md's forward-compat policy.
	Reviewed map[string]bool `json:"reviewed,omitempty"`
}

// GuardrailHit records a tripped guardrail rule against a worktree/file. Line
// is set only by content rules (added_pattern, entropy): the first matching
// added line's NewNum. Per the non-echo invariant (P5-design.md §1.1),
// neither this nor Message ever carries the matched content itself.
type GuardrailHit struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"` // "warn" | "danger"
	Message  string `json:"message"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// EventType enumerates the deltas pushed on the event bus. Payloads stay small:
// clients pull full diffs by id, they are never shipped on the bus.
type EventType string

const (
	EventWorktreeUpserted EventType = "worktree.upserted"
	EventWorktreeRemoved  EventType = "worktree.removed"
	EventDiffReady        EventType = "diff.ready"
	EventGuardrail        EventType = "guardrail.tripped"
	EventReviewChanged    EventType = "review.changed"
	EventCommentChanged   EventType = "comment.changed"
)

// Event is one item on the bus.
type Event struct {
	Type     EventType     `json:"type"`
	Worktree *Worktree     `json:"worktree,omitempty"`
	ID       string        `json:"id,omitempty"`
	Hash     string        `json:"hash,omitempty"`
	Hit      *GuardrailHit `json:"hit,omitempty"`
	At       time.Time     `json:"at"`
}

// ApproveResult summarises a completed approve: the engine's only mutation.
// It lives here (not in internal/engine, where it originated) so the shared
// client package can decode it without importing the engine — a thin client
// binary must never drag gitbackend/discovery/etc. in with it. See
// internal/engine.ApproveResult, kept as a type alias for source compat.
type ApproveResult struct {
	WorktreeID string `json:"worktreeId"`
	Merged     string `json:"merged"`  // feature branch
	Into       string `json:"into"`    // base branch
	Removed    string `json:"removed"` // removed worktree path
}

// Comment is an inline review note anchored to a file (and optionally a
// specific line) within a worktree's diff, for humans and agents to leave
// each other. It lives here, not internal/store (where the v0.1 shape
// originated) — it is a wire type serialised over the API and consumed by
// every frontend, exactly like Worktree/Diff/ApproveResult. See
// .claude/company/handoffs/P4-design.md §1.5 for the frozen shape.
type Comment struct {
	ID         string    `json:"id"` // "c-" + 16 hex, crypto/rand, engine-assigned
	WorktreeID string    `json:"worktreeId"`
	File       string    `json:"file"` // worktree-relative, == DiffFile.Path
	Line       int       `json:"line"` // 0 = file-level comment
	Side       string    `json:"side"` // "new" (default) | "old"; which numbering Line uses
	Body       string    `json:"body"`
	Author     string    `json:"author"`
	State      string    `json:"state"`    // "open" | "resolved"
	FileHash   string    `json:"fileHash"` // DiffFile.Hash when the comment was created
	At         time.Time `json:"at"`
}

// CommentView is a Comment enriched with drift computed at read time against
// the worktree's *current* diff — never persisted. Stale means the file is
// still in the diff but its content (and so its hash) moved since the
// comment was made; Orphaned means the file left the diff entirely. Neither
// ever auto-resolves or auto-deletes the comment — both just surface as a
// badge so a human or agent can decide whether the anchor still applies.
type CommentView struct {
	Comment
	Stale    bool `json:"stale"`
	Orphaned bool `json:"orphaned"`
}

// CommentsPayload is GET /api/comments's response shape and `wt comments
// --json`'s frozen output — the CLI decodes this exact type and re-emits it
// with json.MarshalIndent, so the API and the CLI can never drift apart.
// Path/Branch/Base come from the registry: what lets an agent locate the
// worktree and open file:line directly from the JSON alone.
type CommentsPayload struct {
	WorktreeID string        `json:"worktreeId"`
	Path       string        `json:"path"`
	Branch     string        `json:"branch"`
	Base       string        `json:"base"`
	Comments   []CommentView `json:"comments"`
}
