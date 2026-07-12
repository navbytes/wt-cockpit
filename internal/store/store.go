// Package store persists review state and comments. The Store interface keeps the
// engine decoupled from the backing technology: the MVP uses a JSON file (zero
// dependencies, single static binary preserved), and a SQLite implementation can
// drop in later behind the same interface without touching callers.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Comment is an inline review note a user leaves for an agent to act on.
type Comment struct {
	WorktreeID string    `json:"worktreeId"`
	File       string    `json:"file"`
	Line       int       `json:"line"`
	Body       string    `json:"body"`
	State      string    `json:"state"` // "open" | "resolved"
	At         time.Time `json:"at"`
}

// Store is the persistence contract.
type Store interface {
	SetReviewed(worktreeID, file string, reviewed bool) error
	ReviewedFiles(worktreeID string) (map[string]bool, error)
	ReviewedCount(worktreeID string) int
	ClearWorktree(worktreeID string) error
	AddComment(c Comment) error
	Comments(worktreeID string) ([]Comment, error)
}

// jsonStore is a mutex-guarded, file-backed Store. Writes are atomic (temp+rename).
type jsonStore struct {
	mu   sync.Mutex
	path string
	data persisted
}

type persisted struct {
	// Reviewed[worktreeID][file] = true
	Reviewed map[string]map[string]bool `json:"reviewed"`
	Comments map[string][]Comment       `json:"comments"`
}

// OpenJSON loads (or creates) a JSON-backed store at path.
func OpenJSON(path string) (Store, error) {
	s := &jsonStore{path: path, data: persisted{
		Reviewed: map[string]map[string]bool{},
		Comments: map[string][]Comment{},
	}}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &s.data)
		if s.data.Reviewed == nil {
			s.data.Reviewed = map[string]map[string]bool{}
		}
		if s.data.Comments == nil {
			s.data.Comments = map[string][]Comment{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *jsonStore) SetReviewed(worktreeID, file string, reviewed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.data.Reviewed[worktreeID]
	if m == nil {
		m = map[string]bool{}
		s.data.Reviewed[worktreeID] = m
	}
	if reviewed {
		m[file] = true
	} else {
		delete(m, file)
	}
	return s.flush()
}

func (s *jsonStore) ReviewedFiles(worktreeID string) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for k, v := range s.data.Reviewed[worktreeID] {
		out[k] = v
	}
	return out, nil
}

func (s *jsonStore) ReviewedCount(worktreeID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data.Reviewed[worktreeID])
}

func (s *jsonStore) ClearWorktree(worktreeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Reviewed, worktreeID)
	return s.flush()
}

func (s *jsonStore) AddComment(c Comment) error {
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

func (s *jsonStore) Comments(worktreeID string) ([]Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.data.Comments[worktreeID]
	out := make([]Comment, len(src))
	copy(out, src)
	return out, nil
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
