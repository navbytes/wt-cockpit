// open.go implements store.Open (P6-design.md §7/§4): extension-based
// backend dispatch (".json" → the JSON escape hatch, forever; anything else
// → SQLite), the one-time JSON→SQLite import that runs only when a SQLite
// db is being created (§4.2), and the corrupt-db move-aside safety net
// (§4.3) that sqlite.go's OpenSQLite deliberately leaves to this layer.
package store

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Open dispatches on path's extension: a path ending in ".json" selects the
// JSON store (unchanged forever, until a v1.0 deprecation) — a user who
// explicitly configured "-state …/foo.json" keeps exactly today's behavior,
// zero breakage, no import. Anything else opens (or creates) the SQLite
// store, importing a sibling JSON file when the db file is being created.
func Open(path string) (Store, error) {
	if filepath.Ext(path) == ".json" {
		return OpenJSON(path)
	}
	return openSQLiteWithImport(path)
}

// openSQLiteWithImport opens the SQLite store at path, recovering from a
// corrupt existing file (§4.3) and running the one-time JSON import (§4.2)
// exactly when path's db file did not exist before this call — the same
// stateless trigger in both the "genuinely fresh" and "recovered from
// corruption" cases, since after move-aside the fresh file is, from here,
// indistinguishable from a first-ever run.
func openSQLiteWithImport(path string) (Store, error) {
	existed := fileExists(path)

	st, err := OpenSQLite(path)
	if err != nil {
		if !existed {
			// Nothing pre-existing to blame or move aside: a fresh file that
			// still fails to open is a real error (bad path, permissions).
			return nil, err
		}
		backup, rerr := moveAsideCorruptDB(path, err)
		if rerr != nil {
			return nil, rerr
		}
		// This is *stronger* than the pre-v0.6 JSON behavior of silently
		// zeroing corrupt state on load: the evidence survives under the
		// backup name, and the failure is loud, not silent.
		slog.Error("state db failed to open (corrupt or unreadable); moved aside, starting fresh",
			"path", path, "backup", backup, "error", err)
		existed = false
		if st, err = OpenSQLite(path); err != nil {
			return nil, err
		}
	}

	if !existed {
		s := st.(*sqliteStore)
		if err := runImport(path, s); err != nil {
			s.Close()
			removeDBFiles(path)
			return nil, fmt.Errorf("state db: importing %s failed, refusing to start with a half-migrated database: %w",
				jsonSiblingPath(path), err)
		}
	}
	return st, nil
}

// runImport performs the one-time JSON→SQLite import (§4.2) for a freshly
// created db at dbPath. No sibling state.json is not an error (fresh
// install, step 2); bytes that don't even parse as JSON get OpenJSON's own
// silent-tolerance treatment, just logged instead of silent (step 7); a
// well-formed file is imported in one transaction and, on success, renamed
// to its .imported backup (step 5) — never deleted.
func runImport(dbPath string, s *sqliteStore) error {
	jsonPath := jsonSiblingPath(dbPath)

	raw, err := os.ReadFile(jsonPath)
	switch {
	case os.IsNotExist(err):
		return nil // fresh install: nothing to import, not an error
	case err != nil:
		return err // a real I/O error (permissions, ...) is not "malformed JSON"
	}

	data, perr := tolerantUnmarshal(raw)
	if perr != nil {
		// Matches OpenJSON's own tolerance of unparseable bytes (it discards
		// this same error) — the difference here is visibility, not outcome:
		// a WARN, not silence. The file is left exactly as-is (not renamed)
		// so a human can still recover or fix it later.
		slog.Warn("state.json is not valid JSON; starting with an empty database (file left in place for inspection)",
			"path", jsonPath, "error", perr)
		return nil
	}

	reviews, comments, err := s.importPersisted(data)
	if err != nil {
		return err
	}

	renamed := jsonPath + ".imported"
	if err := os.Rename(jsonPath, renamed); err != nil {
		// The import itself already committed — data is safe — but the
		// rename bookkeeping failed (e.g. permissions). Not fatal: the db is
		// authoritative either way, this just loses the tidy "already
		// imported" evidence trail.
		slog.Warn("state.json imported successfully but could not be renamed to its backup name",
			"path", jsonPath, "want", renamed, "error", err)
		return nil
	}
	slog.Info("imported legacy state.json into sqlite",
		"reviewMarks", reviews, "comments", comments, "source", jsonPath, "backup", renamed)
	return nil
}

// importPersisted bulk-inserts every review mark and comment from data into
// s in one transaction (§4.2 step 4): either every row lands or none does.
// It bypasses AddComment's per-worktree cap and defaulting — this is a
// faithful transplant of already-persisted data, not a new write, and
// comments must never be silently dropped for capacity reasons a prior
// version never enforced against them.
func (s *sqliteStore) importPersisted(data persisted) (reviewCount, commentCount int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	setReviewed := tx.Stmt(s.setReviewedStmt)
	insertComment := tx.Stmt(s.insertCommentStmt)

	for wt, files := range data.ReviewedFiles {
		for path, hash := range files {
			if _, err := setReviewed.Exec(wt, path, hash); err != nil {
				return 0, 0, err
			}
			reviewCount++
		}
	}
	// Comments key on the outer map's worktree bucket (matching jsonStore's
	// own Comments(worktreeID) lookup, s.data.Comments[worktreeID]) rather
	// than each comment's own WorktreeID field — always the same value in
	// practice, but the bucket key is the one semantically load-bearing for
	// "which worktree can retrieve this comment".
	for wt, cs := range data.Comments {
		for _, c := range cs {
			if _, err := insertComment.Exec(
				c.ID, wt, c.File, c.Line, c.Side, c.Body, c.Author, c.State, c.FileHash,
				c.At.Format(time.RFC3339Nano),
			); err != nil {
				return 0, 0, err
			}
			commentCount++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return reviewCount, commentCount, nil
}

// jsonSiblingPath returns path's sibling with a .json extension — where
// v0.1-v0.5 wrote state.json by default (e.g. .../state.db -> .../state.json).
func jsonSiblingPath(dbPath string) string {
	return strings.TrimSuffix(dbPath, filepath.Ext(dbPath)) + ".json"
}

// fileExists reports whether path names an existing filesystem entry. Used
// before OpenSQLite to capture "did the db file exist prior to this Open
// call" — the stateless trigger §4.2 requires (no "already imported" flag
// anywhere).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// moveAsideCorruptDB renames a db file that failed to open (quick_check, a
// bad migration, or a lower-level open error) to a timestamped backup
// (§4.3) — never deleted, so forensics survive. WAL/SHM siblings move with
// it on a best-effort basis; their absence is not an error.
func moveAsideCorruptDB(path string, openErr error) (backup string, err error) {
	backup = fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	if err := os.Rename(path, backup); err != nil {
		return "", fmt.Errorf("state db open failed (%v) and the file could not be moved aside: %w", openErr, err)
	}
	_ = os.Rename(path+"-wal", backup+"-wal")
	_ = os.Rename(path+"-shm", backup+"-shm")
	return backup, nil
}

// removeDBFiles deletes path and its WAL/SHM siblings — used to unwind a
// freshly created db whose one-time import failed transactionally (§4.2
// step 6), so the next start retries cleanly against the (untouched) source
// JSON rather than finding a half-migrated db.
func removeDBFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}
