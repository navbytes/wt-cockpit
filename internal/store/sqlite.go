// sqlite.go is the SQLite-backed Store implementation (P6-design.md
// §2-§5): a drop-in behind Store — same interface, same semantics (§1.1),
// same goroutine-safety (§5) as jsonStore. Not wired into cmd/wtd yet (the
// -state extension dispatch and JSON→SQLite import are WP2); this file is
// the impl plus its parity proof (conformance_test.go, sqlite_test.go).
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers driver "sqlite"; see go.mod's libc-pin comment

	"github.com/navbytes/wt-cockpit/internal/model"
)

// migrations holds forward-only schema DDL (§4.1): migrations[0] takes a
// fresh (user_version 0) db to version 1, migrations[1] would take 1→2, and
// so on. No down migrations pre-1.0 — "down" is restoring a backup.
var migrations = []string{
	// v1: reviewed_files (one row per (worktree, path) review mark) and
	// comments (append-only, read back ordered by seq).
	`
	CREATE TABLE reviewed_files (
		worktree_id TEXT NOT NULL,
		path        TEXT NOT NULL,
		hash        TEXT NOT NULL,
		PRIMARY KEY (worktree_id, path)
	) WITHOUT ROWID;

	CREATE TABLE comments (
		-- seq is a plain rowid alias, not AUTOINCREMENT: SQLite only ever
		-- reuses a rowid at the max end, which is exactly the slot a new
		-- insert occupies in insertion order anyway (a delete-then-add can
		-- only free up the *last* row's number) — "ORDER BY seq" below
		-- never observes a violated insertion order (P6-design.md §3).
		seq         INTEGER PRIMARY KEY,
		id          TEXT NOT NULL,
		worktree_id TEXT NOT NULL,
		file        TEXT NOT NULL,
		line        INTEGER NOT NULL,
		side        TEXT NOT NULL,
		body        TEXT NOT NULL,
		author      TEXT NOT NULL,
		state       TEXT NOT NULL,
		file_hash   TEXT NOT NULL,
		at          TEXT NOT NULL
	);
	CREATE INDEX comments_worktree ON comments(worktree_id);
	`,
}

// sqliteStore is a *sql.DB-backed Store. Concurrency (§5): one connection
// (SetMaxOpenConns(1)) makes database/sql itself queue every call in FIFO
// order — that queueing IS the mutex jsonStore uses; no extra
// synchronization is layered on top here.
type sqliteStore struct {
	db *sql.DB

	setReviewedStmt    *sql.Stmt
	unreviewStmt       *sql.Stmt
	reviewedFilesStmt  *sql.Stmt
	clearReviewedStmt  *sql.Stmt
	clearCommentsStmt  *sql.Stmt
	countCommentsStmt  *sql.Stmt
	insertCommentStmt  *sql.Stmt
	selectCommentsStmt *sql.Stmt
	resolveCommentStmt *sql.Stmt
	deleteCommentStmt  *sql.Stmt
}

// OpenSQLite opens (or creates) a SQLite-backed store at path.
func OpenSQLite(path string) (Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	// Pragmas travel via the DSN (not a per-connection Exec) so they survive
	// any reconnect (§5). busy_timeout covers a brief external lock (e.g. a
	// `sqlite3 state.db` inspection); WAL decouples such a reader from our
	// writes; synchronous=NORMAL is durable at WAL-checkpoint — strictly
	// stronger than jsonStore's no-fsync temp+rename.
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := quickCheck(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	s := &sqliteStore{db: db}
	if err := s.prepare(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// quickCheck runs SQLite's integrity check once at open (sub-ms at our
// sizes, §4.3/§6.1). A corrupt file surfaces here as a loud open error;
// moving the file aside and starting fresh is store.Open's job (WP2), not
// this constructor's.
func quickCheck(db *sql.DB) error {
	var result string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&result); err != nil {
		return fmt.Errorf("state db integrity check failed: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("state db failed integrity check: %s", result)
	}
	return nil
}

// migrate applies migrations[user_version:] in order, each in its own
// transaction ending with the new PRAGMA user_version (§4.1). A fresh file
// starts at user_version 0 — no special case. Refuses to open a db written
// by a newer wtd rather than silently degrading it.
func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("state db was written by a newer wtd (schema v%d, this build supports v%d)", version, len(migrations))
	}
	for _, ddl := range migrations[version:] {
		version++
		if err := applyMigration(db, ddl, version); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(db *sql.DB, ddl string, version int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(ddl); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return err
	}
	return tx.Commit()
}

// prepare compiles every statement once at open (§3) — paid exactly once
// per store lifetime, not once per call.
func (s *sqliteStore) prepare() error {
	stmts := []struct {
		dst  **sql.Stmt
		text string
	}{
		{&s.setReviewedStmt, `INSERT INTO reviewed_files(worktree_id, path, hash) VALUES(?,?,?)
			ON CONFLICT(worktree_id, path) DO UPDATE SET hash=excluded.hash`},
		{&s.unreviewStmt, `DELETE FROM reviewed_files WHERE worktree_id=? AND path=?`},
		{&s.reviewedFilesStmt, `SELECT path, hash FROM reviewed_files WHERE worktree_id=?`},
		{&s.clearReviewedStmt, `DELETE FROM reviewed_files WHERE worktree_id=?`},
		{&s.clearCommentsStmt, `DELETE FROM comments WHERE worktree_id=?`},
		{&s.countCommentsStmt, `SELECT COUNT(*) FROM comments WHERE worktree_id=?`},
		{&s.insertCommentStmt, `INSERT INTO comments(id, worktree_id, file, line, side, body, author, state, file_hash, at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`},
		{&s.selectCommentsStmt, `SELECT id, worktree_id, file, line, side, body, author, state, file_hash, at
			FROM comments WHERE worktree_id=? ORDER BY seq`},
		{&s.resolveCommentStmt, `UPDATE comments SET state='resolved' WHERE worktree_id=? AND id=?`},
		{&s.deleteCommentStmt, `DELETE FROM comments WHERE worktree_id=? AND id=?`},
	}
	for _, st := range stmts {
		p, err := s.db.Prepare(st.text)
		if err != nil {
			return fmt.Errorf("prepare %q: %w", st.text, err)
		}
		*st.dst = p
	}
	return nil
}

// Close releases the underlying connection (checkpointing the WAL — the
// last-connection-close behavior). Deliberately not on Store: callers that
// need it (tests here; store.Open/cmd/wtd in later WPs) hold the concrete
// type.
func (s *sqliteStore) Close() error {
	return s.db.Close()
}

func (s *sqliteStore) SetReviewed(worktreeID, file, hash string) error {
	_, err := s.setReviewedStmt.Exec(worktreeID, file, hash)
	return err
}

// Unreview deletes; an unknown worktree/file affects zero rows, which is
// success (§1.2 #3) — unlike ResolveComment/DeleteComment below,
// RowsAffected is never even checked here.
func (s *sqliteStore) Unreview(worktreeID, file string) error {
	_, err := s.unreviewStmt.Exec(worktreeID, file)
	return err
}

func (s *sqliteStore) ReviewedFiles(worktreeID string) (map[string]string, error) {
	rows, err := s.reviewedFilesStmt.Query(worktreeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{} // non-nil even for an unknown worktree (§1.2 #2)
	for rows.Next() {
		var path, hash string
		if err := rows.Scan(&path, &hash); err != nil {
			return nil, err
		}
		out[path] = hash
	}
	return out, rows.Err()
}

// ClearWorktree wipes both tables for worktreeID in one transaction; an
// unknown worktree affects zero rows in both, which is success (§1.2 #3),
// exactly like Unreview.
func (s *sqliteStore) ClearWorktree(worktreeID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Stmt(s.clearReviewedStmt).Exec(worktreeID); err != nil {
		return err
	}
	if _, err := tx.Stmt(s.clearCommentsStmt).Exec(worktreeID); err != nil {
		return err
	}
	return tx.Commit()
}

// AddComment defaults State/At exactly as jsonStore does (§1.2 #4) before
// persisting. The per-worktree cap (ErrTooManyComments, store.go) is
// existing jsonStore behavior the SQLite impl must not regress on; the
// count-then-insert runs in one transaction so the check can't race a
// concurrent AddComment for the same worktree — the single-connection pool
// serializes whole transactions, matching jsonStore's mutex exactly.
func (s *sqliteStore) AddComment(c model.Comment) error {
	if c.State == "" {
		c.State = "open"
	}
	if c.At.IsZero() {
		c.At = time.Now()
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var n int
	if err := tx.Stmt(s.countCommentsStmt).QueryRow(c.WorktreeID).Scan(&n); err != nil {
		return err
	}
	if n >= maxCommentsPerWorktree {
		return ErrTooManyComments
	}
	if _, err := tx.Stmt(s.insertCommentStmt).Exec(
		c.ID, c.WorktreeID, c.File, c.Line, c.Side, c.Body, c.Author, c.State, c.FileHash,
		c.At.Format(time.RFC3339Nano), // §1.2 #5: RFC3339Nano, original offset preserved on read-back
	); err != nil {
		return err
	}
	return tx.Commit()
}

// Comments returns rows in insertion order (ORDER BY seq, §1.2 #1) as a
// non-nil, possibly-empty slice (§1.2 #2) even for an unknown worktree.
func (s *sqliteStore) Comments(worktreeID string) ([]model.Comment, error) {
	rows, err := s.selectCommentsStmt.Query(worktreeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Comment{}
	for rows.Next() {
		var c model.Comment
		var at string
		if err := rows.Scan(&c.ID, &c.WorktreeID, &c.File, &c.Line, &c.Side, &c.Body, &c.Author, &c.State, &c.FileHash, &at); err != nil {
			return nil, err
		}
		c.At, err = time.Parse(time.RFC3339Nano, at) // preserves the original zone offset (§1.2 #5)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *sqliteStore) ResolveComment(worktreeID, commentID string) error {
	return s.commentWrite(s.resolveCommentStmt, worktreeID, commentID)
}

func (s *sqliteStore) DeleteComment(worktreeID, commentID string) error {
	return s.commentWrite(s.deleteCommentStmt, worktreeID, commentID)
}

// commentWrite runs stmt(worktreeID, commentID) and maps "affected zero
// rows" to ErrCommentNotFound — the contract Resolve/Delete share, distinct
// from Unreview/ClearWorktree's silent no-op above.
func (s *sqliteStore) commentWrite(stmt *sql.Stmt, worktreeID, commentID string) error {
	res, err := stmt.Exec(worktreeID, commentID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrCommentNotFound
	}
	return nil
}
