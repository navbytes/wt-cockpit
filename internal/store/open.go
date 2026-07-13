// open.go implements store.Open (P6-design.md §7/§4): extension-based
// backend dispatch (".json" → the JSON escape hatch, forever; anything else
// → SQLite), the one-time JSON→SQLite import that runs only when a SQLite
// db is being created (§4.2), and the corrupt-db move-aside safety net
// (§4.3) that sqlite.go's OpenSQLite deliberately leaves to this layer.
package store

import (
	"database/sql"
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
// corrupt existing file (§4.3) and running the one-time JSON import (§4.2).
//
// The import itself (runImport) is safe to call unconditionally on every
// Open, not gated by this function's own "did the file exist before this
// call" snapshot (DEFECT D2 fix, P6-fixes.md): that per-call, non-
// transactional existed check used to be the trigger for the one-time
// import, which meant several callers racing a brand-new path could each
// independently observe "did not exist" and each attempt the import against
// the still-present source, duplicating every comment (reviewed_files
// happened to survive the same race unscathed, via its upsert's real
// primary key). runImport now gates itself on a sentinel row checked and
// set inside its own transaction, so it is correct regardless of how many
// times or from how many racing callers it's invoked. existed is still used
// below for its original, unrelated purpose: deciding whether an OpenSQLite
// failure should be treated as a real error or as possible corruption worth
// moving aside.
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
		if st, err = OpenSQLite(path); err != nil {
			return nil, err
		}
	}

	s := st.(*sqliteStore)
	if err := runImport(path, s); err != nil {
		s.Close()
		removeDBFiles(path)
		return nil, fmt.Errorf("state db: importing %s failed, refusing to start with a half-migrated database: %w",
			jsonSiblingPath(path), err)
	}
	return st, nil
}

// runImport performs the one-time JSON→SQLite import (§4.2) for dbPath's
// store s. DEFECT D2 fix (P6-fixes.md): the "has this db already been
// imported into" decision is a sentinel row (import_done, sqlite.go's
// schema) checked and, if clear, claimed by inserting into it — all inside
// ONE immediate transaction (OpenSQLite's DSN sets _txlock=immediate) along
// with the import itself. A second call racing the very first one (two
// goroutines, two processes, or a restart racing a slow-to-exit prior
// instance) blocks on the write lock until the first's transaction commits,
// then observes the claimed sentinel and returns having done nothing —
// exactly-once, regardless of how many callers race it. No sibling
// state.json is not an error (fresh install, step 2); bytes that don't even
// parse as JSON get OpenJSON's own silent-tolerance treatment, just logged
// instead of silent (step 7); a well-formed file is imported in the same
// transaction and, on success, renamed to its .imported backup (step 5) —
// never deleted.
func runImport(dbPath string, s *sqliteStore) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var alreadyRan int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM import_done`).Scan(&alreadyRan); err != nil {
		return err
	}
	if alreadyRan > 0 {
		return nil // a prior call (this process or a racing one) already claimed the one-time import
	}
	if _, err := tx.Exec(`INSERT INTO import_done(done) VALUES(1)`); err != nil {
		return err
	}

	jsonPath := jsonSiblingPath(dbPath)
	raw, err := os.ReadFile(jsonPath)
	switch {
	case os.IsNotExist(err):
		return tx.Commit() // fresh install: nothing to import, not an error; still claims the sentinel
	case err != nil:
		return err // a real I/O error (permissions, ...) is not "malformed JSON" — rolls back
	}

	data, perr := tolerantUnmarshal(raw)
	if perr != nil {
		// Matches OpenJSON's own tolerance of unparseable bytes (it discards
		// this same error) — the difference here is visibility, not outcome:
		// a WARN, not silence. The file is left exactly as-is (not renamed)
		// so a human can still recover or fix it later.
		slog.Warn("state.json is not valid JSON; starting with an empty database (file left in place for inspection)",
			"path", jsonPath, "error", perr)
		return tx.Commit()
	}

	reviews, comments, dupes, err := s.importPersistedTx(tx, data)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if dupes > 0 {
		// DEFECT D1 fix: a hand-edited/hand-merged state.json that itself
		// carries two comments sharing an id (never dropped — the first
		// occurrence lands; see importPersistedTx) — never silent, but never
		// fails the whole import over it either.
		slog.Warn("state.json contained duplicate comment ids; kept the first occurrence of each and discarded the rest",
			"path", jsonPath, "duplicateCommentIds", dupes)
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

// importPersistedTx bulk-inserts every review mark and comment from data
// into tx — an already-open transaction owned by the caller (runImport,
// DEFECT D2 fix: folded into the caller's single atomic import transaction
// rather than opening its own) — §4.2 step 4: either every row lands or
// none does. It bypasses AddComment's per-worktree cap and defaulting —
// this is a faithful transplant of already-persisted data, not a new write,
// and comments must never be silently dropped for capacity reasons a prior
// version never enforced against them.
//
// Comments insert via INSERT OR IGNORE (DEFECT D1 fix): comments.id is
// UNIQUE (sqlite.go), so a source state.json that itself carries two
// comments sharing an id keeps only the first and silently discards the
// rest at the SQL level — dupes counts how many were discarded, for the
// caller to log (never silent, never fails the whole import over one bad
// id, never drops the surviving occurrence).
func (s *sqliteStore) importPersistedTx(tx *sql.Tx, data persisted) (reviewCount, commentCount, dupes int, err error) {
	setReviewed := tx.Stmt(s.setReviewedStmt)
	insertComment := tx.Stmt(s.insertCommentIgnoreStmt)

	for wt, files := range data.ReviewedFiles {
		for path, hash := range files {
			if _, err := setReviewed.Exec(wt, path, hash); err != nil {
				return 0, 0, 0, err
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
			res, err := insertComment.Exec(
				c.ID, wt, c.File, c.Line, c.Side, c.Body, c.Author, c.State, c.FileHash,
				c.At.Format(time.RFC3339Nano),
			)
			if err != nil {
				return 0, 0, 0, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return 0, 0, 0, err
			}
			if n == 0 {
				dupes++ // id already present (earlier in this same source, or an earlier run) — first occurrence wins
				continue
			}
			commentCount++
		}
	}
	return reviewCount, commentCount, dupes, nil
}

// jsonSiblingPath returns path's sibling with a .json extension — where
// v0.1-v0.5 wrote state.json by default (e.g. .../state.db -> .../state.json).
func jsonSiblingPath(dbPath string) string {
	return strings.TrimSuffix(dbPath, filepath.Ext(dbPath)) + ".json"
}

// fileExists reports whether path names an existing filesystem entry. Used
// before OpenSQLite solely to decide whether an open failure means "a real
// error" (nothing existed to fail) or "possibly corrupt, worth moving
// aside" — no longer the import trigger (that moved to a db-internal
// sentinel, DEFECT D2 fix, runImport in this file).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// moveAsideCorruptDB renames a db file that failed to open (quick_check, a
// bad migration, or a lower-level open error) to a timestamped backup
// (§4.3) — never deleted, so forensics survive. WAL/SHM siblings move with
// it on a best-effort basis; their absence is not an error. UnixNano (not
// Unix): two recoveries of the same path within the same wall-clock second
// must not collide and silently overwrite an earlier forensic copy
// (reviewer NIT, P6-fixes.md) — see
// TestMoveAsideCorruptDBUsesUniqueBackupNamesEvenWithinTheSameSecond.
func moveAsideCorruptDB(path string, openErr error) (backup string, err error) {
	backup = fmt.Sprintf("%s.corrupt-%d", path, time.Now().UnixNano())
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
