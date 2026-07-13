// Package store persists review state and comments. The Store interface keeps the
// engine decoupled from the backing technology: the MVP uses a JSON file (zero
// dependencies, single static binary preserved), and a SQLite implementation can
// drop in later behind the same interface without touching callers.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// ErrCommentNotFound is returned by ResolveComment/DeleteComment when
// commentID doesn't exist within worktreeID's comments.
var ErrCommentNotFound = errors.New("comment not found")

// Store is the persistence contract. Review identity is per file: SetReviewed
// records the diff hash the file had when it was reviewed, and a caller decides
// separately (by comparing against the file's current hash) whether that mark
// still holds.
//
// Comment moved to internal/model (P4-design.md §1.5): it is a wire type
// every frontend consumes, like model.ApproveResult. The shipped v0.1 shape
// here (WorktreeID, File, Line, Body, State, At) had zero production callers
// (only this package's own tests), so it carries forward extended in place —
// no migration burden, see TestLoadV03StateFileWithoutNewCommentFieldsStillLoads.
type Store interface {
	SetReviewed(worktreeID, file, hash string) error
	Unreview(worktreeID, file string) error
	ReviewedFiles(worktreeID string) (map[string]string, error)
	ClearWorktree(worktreeID string) error
	AddComment(c model.Comment) error
	Comments(worktreeID string) ([]model.Comment, error)
	ResolveComment(worktreeID, commentID string) error
	DeleteComment(worktreeID, commentID string) error
}

// jsonStore is a mutex-guarded, file-backed Store. Writes are atomic (temp+rename).
type jsonStore struct {
	mu   sync.Mutex
	path string
	data persisted
}

type persisted struct {
	// ReviewedFiles[worktreeID][path] = the diff hash path had when it was last
	// marked reviewed.
	//
	// v0.1 stored a bool under a "reviewed" key instead; that key is simply not
	// read by this struct, so loading an old state file drops old review marks
	// rather than erroring — the whole of that migration.
	ReviewedFiles map[string]map[string]string `json:"reviewed_files"`
	Comments      map[string][]model.Comment   `json:"comments"`
}

// OpenJSON loads (or creates) a JSON-backed store at path.
func OpenJSON(path string) (Store, error) {
	s := &jsonStore{path: path, data: persisted{
		ReviewedFiles: map[string]map[string]string{},
		Comments:      map[string][]model.Comment{},
	}}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &s.data)
		if s.data.ReviewedFiles == nil {
			s.data.ReviewedFiles = map[string]map[string]string{}
		}
		if s.data.Comments == nil {
			s.data.Comments = map[string][]model.Comment{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *jsonStore) SetReviewed(worktreeID, file, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.data.ReviewedFiles[worktreeID]
	if m == nil {
		m = map[string]string{}
		s.data.ReviewedFiles[worktreeID] = m
	}
	m[file] = hash
	return s.flush()
}

func (s *jsonStore) Unreview(worktreeID, file string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.ReviewedFiles[worktreeID], file)
	return s.flush()
}

func (s *jsonStore) ReviewedFiles(worktreeID string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.data.ReviewedFiles[worktreeID] {
		out[k] = v
	}
	return out, nil
}

// ClearWorktree wipes both review state and comments for worktreeID — a
// worktree's comments must not outlive the worktree itself (engine.Approve's
// post-merge cleanup is the only production caller, P4-design.md §1.5).
func (s *jsonStore) ClearWorktree(worktreeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.ReviewedFiles, worktreeID)
	delete(s.data.Comments, worktreeID)
	return s.flush()
}

func (s *jsonStore) AddComment(c model.Comment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.State == "" {
		c.State = "open"
	}
	if c.At.IsZero() {
		c.At = time.Now()
	}
	s.data.Comments[c.WorktreeID] = append(s.data.Comments[c.WorktreeID], c)
	return s.flush()
}

func (s *jsonStore) Comments(worktreeID string) ([]model.Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.data.Comments[worktreeID]
	out := make([]model.Comment, len(src))
	copy(out, src)
	return out, nil
}

// ResolveComment flips a comment's state to "resolved" in place.
func (s *jsonStore) ResolveComment(worktreeID, commentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.data.Comments[worktreeID]
	for i := range list {
		if list[i].ID == commentID {
			list[i].State = "resolved"
			return s.flush()
		}
	}
	return ErrCommentNotFound
}

// DeleteComment removes a comment outright.
func (s *jsonStore) DeleteComment(worktreeID, commentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.data.Comments[worktreeID]
	for i := range list {
		if list[i].ID == commentID {
			s.data.Comments[worktreeID] = append(list[:i], list[i+1:]...)
			return s.flush()
		}
	}
	return ErrCommentNotFound
}

// flush writes the whole state atomically. Caller must hold the mutex.
func (s *jsonStore) flush() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
