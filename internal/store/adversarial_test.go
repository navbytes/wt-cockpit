// adversarial_test.go is a targeted adversarial pass over the v0.6 SQLite
// store + JSON->SQLite migration path (P6-design.md), added on top of the
// existing conformance/sqlite/open/bench suites rather than duplicating
// them: every test here targets a gap those suites didn't already cover
// (checked file-by-file before writing any of this). Where a test revealed a
// genuine backend-parity bug rather than a coverage gap, it originally
// asserted the INTENDED (jsonStore-matching) behavior and skipped with a
// "DEFECT Dn" marker for the backend that failed it — see
// .claude/company/handoffs/P6-tester.md for the original defect cards and
// P6-fixes.md for how D1 (comment id collision) and D2 (concurrent Open
// duplication/raw schema error) were fixed; those tests are un-skipped and
// now assert the fixed behavior directly (D3, engine-scope, is out of this
// fix's remit and stays skipped).
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// commentWithBody returns a pointer to the first entry in cs whose Body
// equals body — used below to identify a specific comment among several
// that deliberately share the same ID (the collision tests), so assertions
// don't depend on incidental slice-index ordering.
func commentWithBody(cs []model.Comment, body string) *model.Comment {
	for i := range cs {
		if cs[i].Body == body {
			return &cs[i]
		}
	}
	return nil
}

// ---- migration fidelity: every field, unicode, injection, control bytes (highest value) ----

// TestOpenImportPreservesUnicodeEmptyAuthorResolvedFileLevelAndOldSide pins
// the migration-fidelity contract at its highest-value point (P6-design.md
// §4.2): a comment exercising every "unusual" field value at once — a
// unicode body, an empty author, state=resolved, a file-level (line 0)
// anchor, the "old" side, and a file hash — must come back byte-identical
// after import, not just field-shape-identical. Built via json.Marshal on
// the real persisted/model.Comment types (not hand-typed JSON text) so the
// fixture can never silently drift from the actual wire shape.
func TestOpenImportPreservesUnicodeEmptyAuthorResolvedFileLevelAndOldSide(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "state.json")
	dbPath := filepath.Join(dir, "state.db")

	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	want := model.Comment{
		ID: "c-unicode", WorktreeID: "wt1", File: "readme.md",
		Line: 0, Side: "old", Body: "héllo — 世界 🚀 café, naïve, Zürich, Ω≠π",
		Author: "", State: "resolved", FileHash: "hash-unicode", At: at,
	}
	src := persisted{
		ReviewedFiles: map[string]map[string]string{},
		Comments:      map[string][]model.Comment{"wt1": {want}},
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, jsonPath, string(raw))

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.(*sqliteStore).Close()

	cs, err := s.Comments("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 {
		t.Fatalf("comments after import = %+v, want exactly 1", cs)
	}
	got := cs[0]
	if got.ID != want.ID || got.File != want.File || got.Line != want.Line ||
		got.Side != want.Side || got.Body != want.Body || got.Author != want.Author ||
		got.State != want.State || got.FileHash != want.FileHash {
		t.Errorf("comment not byte-identical after import:\n got  %+v\n want %+v", got, want)
	}
	if got.At.Format(time.RFC3339Nano) != want.At.Format(time.RFC3339Nano) {
		t.Errorf("At = %v, want %v", got.At, want.At)
	}
}

// TestCommentBodySurvivesSQLInjectionControlBytesEmbeddedNULAndLongBody
// pins the "parameterization proof at the data layer": every write goes
// through a `?` placeholder, never string concatenation, so a body that
// LOOKS like SQL, or carries raw control bytes (including an embedded NUL),
// must round-trip as inert data, not execute or truncate. Run against both
// backends directly via AddComment/Comments (not the import path — that
// gets its own proof below, since importPersisted uses a separate prepared
// statement).
func TestCommentBodySurvivesSQLInjectionControlBytesEmbeddedNULAndLongBody(t *testing.T) {
	injection := "'); DROP TABLE comments; --\n\x01\x02\x1f\x00tail-after-embedded-nul"
	long := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 8000) // ~368 KiB

	for _, b := range storeBackends {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t, statePath(t, b))
			if err := s.AddComment(model.Comment{ID: "c-inj", WorktreeID: "wt1", File: "a.go", Body: injection}); err != nil {
				t.Fatal(err)
			}
			if err := s.AddComment(model.Comment{ID: "c-long", WorktreeID: "wt1", File: "a.go", Body: long}); err != nil {
				t.Fatal(err)
			}

			cs, err := s.Comments("wt1")
			if err != nil {
				t.Fatal(err)
			}
			if len(cs) != 2 {
				t.Fatalf("comments = %d, want 2 — the table must still exist and hold both rows (no injected DROP TABLE took effect)", len(cs))
			}
			inj := commentWithBody(cs, injection)
			if inj == nil {
				t.Errorf("injection-shaped body not preserved verbatim, got %+v", cs)
			}
			longGot := commentWithBody(cs, long)
			if longGot == nil {
				t.Errorf("long body corrupted or truncated (want %d bytes)", len(long))
			}
		})
	}
}

// TestOpenImportPreservesSQLInjectionAndControlBytesVerbatim is the same
// proof against importPersisted specifically (open.go): its bulk insert
// path uses its own prepared statement, not AddComment's, so it needs its
// own verbatim-survival proof rather than inheriting coverage from the
// conformance test above.
func TestOpenImportPreservesSQLInjectionAndControlBytesVerbatim(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "state.json")
	dbPath := filepath.Join(dir, "state.db")

	injection := "'); DROP TABLE comments; --\n\x01\x02\x1f\x00tail-after-embedded-nul"
	src := persisted{
		ReviewedFiles: map[string]map[string]string{},
		Comments: map[string][]model.Comment{"wt1": {
			{ID: "c-inj", WorktreeID: "wt1", File: "a.go", Body: injection, State: "open", At: time.Now()},
		}},
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, jsonPath, string(raw))

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.(*sqliteStore).Close()

	cs, err := s.Comments("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Body != injection {
		t.Errorf("injection-shaped body via import not preserved verbatim: %+v", cs)
	}

	// Belt-and-braces: the comments table itself must still be present —
	// direct proof the embedded "DROP TABLE" text never executed as SQL.
	var name string
	row := s.(*sqliteStore).db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='comments'`)
	if err := row.Scan(&name); err != nil {
		t.Errorf("comments table missing after importing an injection-shaped body: %v", err)
	}
}

// ---- migration idempotency/safety: empty/zero-byte json, racing concurrent opens ----

// TestOpenEmptyJSONObjectSiblingImportsCleanlyAndIsRenamed pins the
// "state.json present but empty" edge: a well-formed-but-empty {} must not
// crash Open, must import nothing, and (being well-formed JSON, just empty)
// still gets renamed to its .imported backup like any other successful
// import.
func TestOpenEmptyJSONObjectSiblingImportsCleanlyAndIsRenamed(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "state.json")
	dbPath := filepath.Join(dir, "state.db")
	writeStateJSON(t, jsonPath, `{}`)

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("an empty {} state.json must not fail Open, got: %v", err)
	}
	defer s.(*sqliteStore).Close()

	cs, _ := s.Comments("anything")
	if len(cs) != 0 {
		t.Errorf("nothing to import from {}, got %+v", cs)
	}
	if _, err := os.Stat(jsonPath + ".imported"); err != nil {
		t.Errorf("a well-formed (if empty) source should still be renamed to its backup: %v", err)
	}
}

// TestOpenZeroByteJSONSiblingIsToleratedNotFatal pins the "state.json
// present but zero bytes" edge, distinct from {}: zero bytes doesn't even
// parse as JSON, so this is OpenJSON's existing malformed-input tolerance
// (logged, not silent, source left in place) — same contract
// TestOpenMalformedJSONSiblingIsToleratedNotFatal already pins for
// `{not valid json`, exercised here at the more extreme "nothing at all"
// end of that same input space.
func TestOpenZeroByteJSONSiblingIsToleratedNotFatal(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "state.json")
	dbPath := filepath.Join(dir, "state.db")
	writeStateJSON(t, jsonPath, "") // zero bytes, not even "{}"

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("a zero-byte state.json must not fail Open, got: %v", err)
	}
	defer s.(*sqliteStore).Close()

	cs, _ := s.Comments("anything")
	if len(cs) != 0 {
		t.Errorf("nothing to import from a zero-byte file, got %+v", cs)
	}
	if _, err := os.Stat(jsonPath); err != nil {
		t.Errorf("zero-byte state.json should be left in place (unparseable, not renamed), stat err: %v", err)
	}
	if _, err := os.Stat(jsonPath + ".imported"); !os.IsNotExist(err) {
		t.Error("no .imported backup should be created for an unparseable zero-byte source")
	}
}

// TestOpenConcurrentFreshOpensNeverDuplicateImportedData simulates the
// migration being "interrupted" by a second racing Open: P6-design.md's
// §4.2 import trigger used to be stateless per call ("did the db file exist
// before THIS call"), not a global once-only flag, so two callers that both
// observed "does not exist" at the same instant could each run the
// one-time import against the same untouched source.
//
// DEFECT D2, FIXED (P6-fixes.md — was confirmed, not just theoretical: at
// n=8 concurrent Opens this used to reliably reproduce real comment
// duplication, observed comment counts of 2x/5x/6x/7x the source count (6,
// 15, 18, 21 for a 3-comment source) across repeated runs). Root cause was
// twofold: (1) openSQLiteWithImport's `existed` check ran once per call,
// before OpenSQLite, so several callers could each independently believe
// (from their own, earlier "did not exist" snapshot) that they must run the
// one-time import; (2) migrate() (sqlite.go) read `PRAGMA user_version` in
// its own autocommit query outside the DDL transaction, so several callers'
// OpenSQLite calls could all succeed without erroring. Fix: the "should I
// import" decision is now a sentinel row (import_done) checked and claimed
// inside the SAME immediate transaction as the import (runImport, open.go),
// and migrate() reads user_version inside its own immediate transaction
// too (DSN _txlock=immediate) — see P6-tester.md DEFECT D2 and
// P6-fixes.md. comments.id is also now UNIQUE (DEFECT D1's fix), a second
// structural backstop against ever landing a duplicate row even if the
// import trigger somehow ran twice.
func TestOpenConcurrentFreshOpensNeverDuplicateImportedData(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "state.json")
	dbPath := filepath.Join(dir, "state.db")

	src := persisted{
		ReviewedFiles: map[string]map[string]string{"wt1": {"a.go": "h1", "b.go": "h2"}},
		Comments: map[string][]model.Comment{"wt1": {
			{ID: "c-1", WorktreeID: "wt1", File: "a.go", Body: "one", State: "open", At: time.Now()},
			{ID: "c-2", WorktreeID: "wt1", File: "a.go", Body: "two", State: "open", At: time.Now()},
			{ID: "c-3", WorktreeID: "wt1", File: "b.go", Body: "three", State: "open", At: time.Now()},
		}},
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, jsonPath, string(raw))

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	stores := make([]Store, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			st, err := Open(dbPath)
			errs[i] = err
			stores[i] = st
		}(i)
	}
	close(start)
	wg.Wait()

	var successes int
	for i, err := range errs {
		if err == nil {
			successes++
			stores[i].(*sqliteStore).Close()
		} else {
			t.Errorf("goroutine %d: Open failed: %v", i, err)
		}
	}
	if successes != n {
		t.Fatalf("%d/%d racing Opens succeeded, want all %d", successes, n, n)
	}

	final := openSQLiteT(t, dbPath)
	cs, err := final.Comments("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 {
		t.Errorf("comments after %d racing Opens = %d, want exactly 3 (source count, no duplication)", n, len(cs))
	}
	rev, err := final.ReviewedFiles("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rev) != 2 {
		t.Errorf("reviewed files after racing Opens = %+v, want exactly {a.go, b.go}", rev)
	}

	// Exactly one winner actually ran the import: the source is gone,
	// renamed to its .imported backup exactly once (not left in place, not
	// renamed more than once — there is only one filesystem entry to check).
	if _, err := os.Stat(jsonPath); !os.IsNotExist(err) {
		t.Errorf("state.json should be gone (renamed away by the single winning import), stat err = %v", err)
	}
	if _, err := os.Stat(jsonPath + ".imported"); err != nil {
		t.Errorf("state.json.imported backup missing: %v", err)
	}
}

// TestConcurrentOpenSQLiteOnFreshPathNeverSurfacesRawSchemaError isolates a
// second symptom of DEFECT D2, at the schema layer rather than the import
// layer: it is NOT specific to the JSON import wrapper (store.Open) — plain
// concurrent OpenSQLite calls against the SAME not-yet-existing path (no
// JSON sibling, nothing to import) used to reproduce it directly. migrate()
// (sqlite.go) used to read `PRAGMA user_version` in its own autocommit
// query, separate from the transaction that then applied the DDL, so two
// racing connections could both observe user_version=0 and both attempt
// `CREATE TABLE` — confirmed deterministically reproducible at n=16
// concurrent opens pre-fix, surfacing modernc's raw driver string
// ("table ... already exists") all the way up through OpenSQLite/store.Open
// instead of either a graceful no-op open or a clean, wtd-styled error.
//
// DEFECT D2, FIXED (P6-fixes.md): migrate() now reads user_version inside
// one immediate transaction (OpenSQLite's DSN sets _txlock=immediate), so
// the write lock is acquired before the read — a racing connection blocks
// for the winner's commit (busy_timeout) and then observes the
// already-current version, applying nothing and erroring never. Every
// concurrent OpenSQLite call must now succeed cleanly. See P6-tester.md
// DEFECT D2, which covers both symptoms (this one, and the previous test's
// confirmed comment duplication) under one root cause.
func TestConcurrentOpenSQLiteOnFreshPathNeverSurfacesRawSchemaError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	stores := make([]Store, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			st, err := OpenSQLite(dbPath)
			errs[i] = err
			stores[i] = st
		}(i)
	}
	close(start)
	wg.Wait()
	defer func() {
		for i, err := range errs {
			if err == nil {
				stores[i].(*sqliteStore).Close()
			}
		}
	}()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error (every racing OpenSQLite on a fresh path must now succeed cleanly): %v", i, err)
		}
	}
}

// ---- backend dispatch gaps ----

func TestOpenDotSQLiteExtensionSelectsSQLiteStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.(*sqliteStore).Close()
	if _, ok := s.(*sqliteStore); !ok {
		t.Fatalf("Open(%q) = %T, want *sqliteStore", path, s)
	}
}

func TestOpenNoExtensionSelectsSQLiteStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state") // no extension at all
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.(*sqliteStore).Close()
	if _, ok := s.(*sqliteStore); !ok {
		t.Fatalf("Open(%q) = %T, want *sqliteStore", path, s)
	}
}

// TestOpenCreatesMissingParentDirectories covers both dispatch targets at a
// path whose containing directory doesn't exist yet (a first-ever run
// writing under a not-yet-created config dir, e.g. ~/.wtcockpit): Open must
// create the chain and hand back a fully usable store, not just avoid
// erroring.
func TestOpenCreatesMissingParentDirectories(t *testing.T) {
	for _, ext := range []string{".db", ".json"} {
		t.Run(ext, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nested", "not-yet-created", "state"+ext)
			s, err := Open(path)
			if err != nil {
				t.Fatalf("Open should create missing parent dirs, got: %v", err)
			}
			if cs, ok := s.(*sqliteStore); ok {
				defer cs.Close()
			}
			if err := s.SetReviewed("wt1", "a.go", "h1"); err != nil {
				t.Fatalf("store at a freshly-created nested dir should be usable: %v", err)
			}
			rev, err := s.ReviewedFiles("wt1")
			if err != nil || rev["a.go"] != "h1" {
				t.Errorf("rev = %+v, err = %v", rev, err)
			}
		})
	}
}

// ---- comment ID collision (backend parity) ----
//
// model.Comment.ID is meant to be unique (crypto/rand, "c-" + 16 hex,
// engine-assigned per its doc comment) but nothing in the Store interface
// enforced that, and a hand-edited or hand-merged state.json could still
// carry a duplicate into an import. jsonStore's ResolveComment/DeleteComment
// (store.go) both do a linear scan that stops at the FIRST id match.
//
// DEFECT D1, FIXED (P6-fixes.md): comments.id is now UNIQUE in the SQLite
// schema (sqlite.go), so a duplicate id can no longer land as a second row
// in the first place — AddComment returns ErrDuplicateCommentID instead.
// That makes the original scenario (two rows sharing an id, only the first
// affected by Resolve/Delete) structurally unreachable for sqlite: there is
// only ever one "c-dup" row to affect, so parity holds by construction
// rather than by matching jsonStore's scan behavior. jsonStore itself is
// unchanged (out of this fix's scope) and still allows the collision, so
// its half of these tests still exercises the original scenario directly.

func TestResolveCommentWithDuplicateIDAffectsOnlyFirstMatch(t *testing.T) {
	for _, b := range storeBackends {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t, statePath(t, b))
			if err := s.AddComment(model.Comment{ID: "c-dup", WorktreeID: "wt1", File: "a.go", Body: "first"}); err != nil {
				t.Fatal(err)
			}
			err := s.AddComment(model.Comment{ID: "c-dup", WorktreeID: "wt1", File: "a.go", Body: "second"})

			if b.name == "sqlite" {
				if !errors.Is(err, ErrDuplicateCommentID) {
					t.Fatalf("AddComment with a duplicate id = %v, want ErrDuplicateCommentID (DEFECT D1 fix: comments.id is UNIQUE)", err)
				}
			} else if err != nil {
				t.Fatal(err) // jsonStore has no such guard (unchanged, pre-existing behavior) — the duplicate must still be accepted
			}

			if err := s.ResolveComment("wt1", "c-dup"); err != nil {
				t.Fatal(err)
			}
			cs, err := s.Comments("wt1")
			if err != nil {
				t.Fatal(err)
			}
			first := commentWithBody(cs, "first")
			if first == nil || first.State != "resolved" {
				t.Fatalf("the comment sharing the id should be resolved, got %+v", cs)
			}

			if b.name == "json" {
				second := commentWithBody(cs, "second")
				if second == nil {
					t.Fatalf("precondition: jsonStore's colliding comment must survive, got %+v", cs)
				}
				if second.State == "resolved" {
					t.Errorf("only the first comment sharing an id should resolve; the second was also resolved (got %+v)", *second)
				}
			} else if len(cs) != 1 {
				t.Errorf("sqlite: the duplicate insert must have been rejected, so exactly one comment should exist, got %+v", cs)
			}
		})
	}
}

func TestDeleteCommentWithDuplicateIDAffectsOnlyFirstMatch(t *testing.T) {
	for _, b := range storeBackends {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t, statePath(t, b))
			if err := s.AddComment(model.Comment{ID: "c-dup", WorktreeID: "wt1", File: "a.go", Body: "first"}); err != nil {
				t.Fatal(err)
			}
			err := s.AddComment(model.Comment{ID: "c-dup", WorktreeID: "wt1", File: "a.go", Body: "second"})

			if b.name == "sqlite" {
				if !errors.Is(err, ErrDuplicateCommentID) {
					t.Fatalf("AddComment with a duplicate id = %v, want ErrDuplicateCommentID (DEFECT D1 fix: comments.id is UNIQUE)", err)
				}
			} else if err != nil {
				t.Fatal(err) // jsonStore has no such guard (unchanged, pre-existing behavior) — the duplicate must still be accepted
			}

			if err := s.DeleteComment("wt1", "c-dup"); err != nil {
				t.Fatal(err)
			}
			cs, err := s.Comments("wt1")
			if err != nil {
				t.Fatal(err)
			}

			if b.name == "sqlite" {
				if len(cs) != 0 {
					t.Errorf("sqlite: the only comment sharing the id should be deleted (the duplicate insert was rejected), got %+v", cs)
				}
				return
			}
			if len(cs) != 1 || cs[0].Body != "second" {
				t.Errorf("only the FIRST comment sharing the id should be deleted; want just %q remaining, got %+v", "second", cs)
			}
		})
	}
}

// ---- sqlite concurrency stress (WP1 flagged this as not yet written) ----

// TestSQLiteConcurrentMixedOpsAreRaceCleanAndLossless hammers ONE shared
// sqliteStore from many goroutines doing SetReviewed/AddComment/
// ReviewedFiles, pinning the MaxOpenConns(1) serialization claim
// (P6-design.md §5, sqlite.go's own doc comment): no call should ever
// surface a "database is locked"/SQLITE_BUSY error, and every write must
// land — the queueing is database/sql's connection-pool FIFO, not a lucky
// race. Intended to run under `go test -race`.
func TestSQLiteConcurrentMixedOpsAreRaceCleanAndLossless(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openSQLiteT(t, path)

	const n = 60 // comfortably under maxCommentsPerWorktree (500)
	var wg sync.WaitGroup
	setErrs := make([]error, n)
	addErrs := make([]error, n)
	readErrs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			file := fmt.Sprintf("file-%d.go", i)
			setErrs[i] = s.SetReviewed("shared-wt", file, fmt.Sprintf("hash-%d", i))
			addErrs[i] = s.AddComment(model.Comment{ID: fmt.Sprintf("c-%d", i), WorktreeID: "shared-wt", File: file, Body: "note"})
			_, readErrs[i] = s.ReviewedFiles("shared-wt")
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if setErrs[i] != nil {
			t.Errorf("goroutine %d: SetReviewed error (SQLITE_BUSY should never surface): %v", i, setErrs[i])
		}
		if addErrs[i] != nil {
			t.Errorf("goroutine %d: AddComment error: %v", i, addErrs[i])
		}
		if readErrs[i] != nil {
			t.Errorf("goroutine %d: ReviewedFiles error: %v", i, readErrs[i])
		}
	}

	rev, err := s.ReviewedFiles("shared-wt")
	if err != nil {
		t.Fatal(err)
	}
	if len(rev) != n {
		t.Errorf("reviewed files after %d concurrent writers = %d, want %d — a write was lost", n, len(rev), n)
	}
	for i := 0; i < n; i++ {
		file := fmt.Sprintf("file-%d.go", i)
		want := fmt.Sprintf("hash-%d", i)
		if rev[file] != want {
			t.Errorf("%s hash = %q, want %q", file, rev[file], want)
		}
	}
	cs, err := s.Comments("shared-wt")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != n {
		t.Errorf("comments after %d concurrent writers = %d, want %d — a comment was lost", n, len(cs), n)
	}
}

// ---- budget-gate skip guarantee ----

// TestRequireBenchGateSkipsUnlessEnvSet pins the §6.3 guarantee directly
// (rather than relying on every TestBudget*'s own incidental skip): with
// WT_BENCH_GATE unset, requireBenchGate must skip before any timing-
// sensitive body runs; with it set, the same call must NOT skip — proving
// this is a real gate, not an unconditional skip that would silently hide a
// broken budget test forever.
func TestRequireBenchGateSkipsUnlessEnvSet(t *testing.T) {
	t.Run("unset_skips", func(t *testing.T) {
		t.Setenv("WT_BENCH_GATE", "")
		requireBenchGate(t)
		t.Fatal("requireBenchGate should have skipped this subtest before reaching here")
	})
	t.Run("set_runs", func(t *testing.T) {
		t.Setenv("WT_BENCH_GATE", "1")
		requireBenchGate(t) // must NOT skip
		// Reaching here (and the subtest reporting non-skipped) proves it didn't.
	})
}
