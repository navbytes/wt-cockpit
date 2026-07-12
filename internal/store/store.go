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

// Store is the persistence contract. Review identity is per file: SetReviewed
// records the diff hash the file had when it was reviewed, and a caller decides
// separately (by comparing against the file's current hash) whether that mark
// still holds.
type Store interface {
	SetReviewed(worktreeID, file, hash string) error
	Unreview(worktreeID, file string) error
	ReviewedFiles(worktreeID string) (map[string]string, error)
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
	// ReviewedFiles[worktreeID][path] = the diff hash path had when it was last
	// marked reviewed.
	//
	// v0.1 stored a bool under a "reviewed" key instead; that key is simply not
	// read by this struct, so loading an old state file drops old review marks
	// rather than erroring — the whole of that migration.
	ReviewedFiles map[string]map[string]string `json:"reviewed_files"`
	Comments      map[string][]Comment         `json:"comments"`
}

// OpenJSON loads (or creates) a JSON-backed store at path.
func OpenJSON(path string) (Store, error) {
	s := &jsonStore{path: path, data: persisted{
		ReviewedFiles: map[string]map[string]string{},
		Comments:      map[string][]Comment{},
	}}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &s.data)
		if s.data.ReviewedFiles == nil {
			s.data.ReviewedFiles = map[string]map[string]string{}
		}
		if s.data.Comments == nil {
			s.data.Comments = map[string][]Comment{}
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

func (s *jsonStore) ClearWorktree(worktreeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.ReviewedFiles, worktreeID)
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
