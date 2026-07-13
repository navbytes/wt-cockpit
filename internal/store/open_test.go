// open_test.go exercises store.Open (P6-design.md §WP2): extension
// dispatch, the one-time JSON→SQLite import and its idempotency, the
// malformed-JSON tolerance, and the corrupt-db move-aside safety net.
package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeStateJSON(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---- dispatch (§7) ----

func TestOpenDotJSONSelectsJSONStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(*jsonStore); !ok {
		t.Fatalf("Open(%q) = %T, want *jsonStore", path, s)
	}
}

func TestOpenDotDBSelectsSQLiteStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.(*sqliteStore).Close()
	if _, ok := s.(*sqliteStore); !ok {
		t.Fatalf("Open(%q) = %T, want *sqliteStore", path, s)
	}
}

// TestOpenDefaultShapedPathSelectsSQLiteStore pins cmd/wtd's actual built-in
// default (~/.wtcockpit/state.db, P6-design.md §7): a caller that never
// overrides -state gets this exact shape, and it must resolve to SQLite,
// not the legacy JSON store.
func TestOpenDefaultShapedPathSelectsSQLiteStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".wtcockpit", "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.(*sqliteStore).Close()
	if _, ok := s.(*sqliteStore); !ok {
		t.Fatalf("Open(%q) = %T, want *sqliteStore (wtd's default backend)", path, s)
	}
}

// ---- one-time import (§4.2) ----

// TestOpenImportsExistingJSONOnFreshCreate is the WP2 headline test: a
// legacy state.json with review marks AND comments (including a resolved
// one, and every Comment field) sitting next to a not-yet-existing .db path
// gets imported in full the first time the db is created, and the source
// file is renamed to its .imported backup on success.
func TestOpenImportsExistingJSONOnFreshCreate(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "state.json")
	dbPath := filepath.Join(dir, "state.db")

	at1 := time.Date(2026, 3, 1, 10, 30, 0, 123456789, time.FixedZone("+05:30", 5*3600+30*60))
	at2 := time.Date(2026, 4, 2, 8, 0, 0, 0, time.UTC)
	raw := fmt.Sprintf(`{
		"reviewed_files": {"wt1": {"a.go": "hash-a", "b.go": "hash-b"}},
		"comments": {"wt1": [
			{"id":"c-1","worktreeId":"wt1","file":"a.go","line":10,"side":"new","body":"fix this","author":"nav","state":"open","fileHash":"hash-a","at":%q},
			{"id":"c-2","worktreeId":"wt1","file":"b.go","line":0,"side":"old","body":"looks resolved","author":"agent","state":"resolved","fileHash":"hash-b","at":%q}
		]}
	}`, at1.Format(time.RFC3339Nano), at2.Format(time.RFC3339Nano))
	writeStateJSON(t, jsonPath, raw)

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.(*sqliteStore).Close()

	rev, err := s.ReviewedFiles("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rev) != 2 || rev["a.go"] != "hash-a" || rev["b.go"] != "hash-b" {
		t.Errorf("reviewed files after import = %+v, want a.go=hash-a, b.go=hash-b", rev)
	}

	cs, err := s.Comments("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("comments after import = %+v, want 2", cs)
	}
	c1, c2 := cs[0], cs[1] // JSON-array (insertion) order preserved
	if c1.ID != "c-1" || c1.File != "a.go" || c1.Line != 10 || c1.Side != "new" ||
		c1.Body != "fix this" || c1.Author != "nav" || c1.State != "open" || c1.FileHash != "hash-a" {
		t.Errorf("comment 1 not byte-faithful: %+v", c1)
	}
	if c1.At.Format(time.RFC3339Nano) != at1.Format(time.RFC3339Nano) {
		t.Errorf("comment 1 At = %v, want %v", c1.At, at1)
	}
	if c2.ID != "c-2" || c2.File != "b.go" || c2.Line != 0 || c2.Side != "old" ||
		c2.Body != "looks resolved" || c2.Author != "agent" || c2.State != "resolved" || c2.FileHash != "hash-b" {
		t.Errorf("comment 2 (resolved) not byte-faithful: %+v", c2)
	}
	if c2.At.Format(time.RFC3339Nano) != at2.Format(time.RFC3339Nano) {
		t.Errorf("comment 2 At = %v, want %v", c2.At, at2)
	}

	if _, err := os.Stat(jsonPath); !os.IsNotExist(err) {
		t.Errorf("state.json should be gone (renamed away), stat err = %v", err)
	}
	if _, err := os.Stat(jsonPath + ".imported"); err != nil {
		t.Errorf("state.json.imported backup missing: %v", err)
	}
}

// TestOpenDoesNotReimportOnSecondOpen pins §4.2's "subsequent opens: the
// import trigger never fires again" — even a brand-new state.json dropped
// in after the db exists must be ignored; the already-migrated db is
// authoritative.
func TestOpenDoesNotReimportOnSecondOpen(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "state.json")
	dbPath := filepath.Join(dir, "state.db")
	writeStateJSON(t, jsonPath, `{"reviewed_files":{},"comments":{"wt1":[
		{"id":"c-1","worktreeId":"wt1","file":"a.go","body":"one","state":"open","at":"2026-01-01T00:00:00Z"}
	]}}`)

	s1, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	s1.(*sqliteStore).Close()

	// A brand-new state.json appears (an old daemon writing again, or a user
	// dropping one in) — the already-migrated db must win regardless.
	writeStateJSON(t, jsonPath, `{"reviewed_files":{},"comments":{"wt1":[
		{"id":"c-2","worktreeId":"wt1","file":"z.go","body":"should be ignored","state":"open","at":"2026-01-01T00:00:00Z"}
	]}}`)

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.(*sqliteStore).Close()

	cs, err := s2.Comments("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].ID != "c-1" {
		t.Errorf("second Open must not re-import; comments = %+v, want just c-1", cs)
	}
}

// TestOpenNoJSONSiblingCreatesFreshEmptyDB pins §4.2 step 2: a fresh install
// with no state.json at all must not error — just an empty, usable db.
func TestOpenNoJSONSiblingCreatesFreshEmptyDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open with no sibling state.json should not error, got: %v", err)
	}
	defer s.(*sqliteStore).Close()

	cs, err := s.Comments("anything")
	if err != nil || len(cs) != 0 {
		t.Errorf("fresh db should be empty, got %+v, err=%v", cs, err)
	}
}

// TestOpenMalformedJSONSiblingIsToleratedNotFatal pins §4.2 step 7. Design
// note on an apparent tension with a looser paraphrase ("fails loudly"): the
// design text is explicit and is authoritative here — malformed JSON gets
// exactly OpenJSON's own long-standing tolerance (store.go's OpenJSON
// silently discards this same unmarshal error today; TestLoadV01/TestLoadV03
// already pin that "start empty" is the contract for bad/old input). The
// only change on the import path is that the tolerance is logged (a WARN),
// not silent, and the source file is deliberately left in place — nothing
// was imported from it, so there is nothing to rename, and a human can still
// recover the original bytes.
func TestOpenMalformedJSONSiblingIsToleratedNotFatal(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "state.json")
	dbPath := filepath.Join(dir, "state.db")
	writeStateJSON(t, jsonPath, `{not valid json at all`)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("malformed state.json must not fail Open (design §4.2 step 7), got: %v", err)
	}
	defer s.(*sqliteStore).Close()

	cs, _ := s.Comments("wt1")
	if len(cs) != 0 {
		t.Errorf("nothing should be imported from unparseable JSON, got %+v", cs)
	}
	rev, _ := s.ReviewedFiles("wt1")
	if len(rev) != 0 {
		t.Errorf("nothing should be imported from unparseable JSON, got %+v", rev)
	}
	if _, err := os.Stat(jsonPath); err != nil {
		t.Errorf("malformed state.json must be left in place (not renamed), stat err: %v", err)
	}
	if _, err := os.Stat(jsonPath + ".imported"); !os.IsNotExist(err) {
		t.Error("no .imported backup should be created for a malformed source")
	}

	var sawWarning bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["level"] == "WARN" && strings.Contains(fmt.Sprint(rec["msg"]), "state.json") {
			sawWarning = true
		}
	}
	if !sawWarning {
		t.Errorf("the tolerance must be logged at WARN (not silent), got log output:\n%s", logs.String())
	}
}

// TestOpenImportsOldShapeJSONWithoutDroppingComments pins the split ruling
// in §4.2: a v0.1-shaped "reviewed" bool key isn't mappable to the new
// per-file hash shape and is dropped (review marks are re-creatable — worst
// case a re-review), but that same file's comments (in the v0.3-era shape
// missing id/side/fileHash) are imported in full — comments are authored
// content and must never be silently dropped.
func TestOpenImportsOldShapeJSONWithoutDroppingComments(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "state.json")
	dbPath := filepath.Join(dir, "state.db")
	raw := `{
		"reviewed": {"wt1": {"a.go": true}},
		"comments": {"wt1": [{"worktreeId":"wt1","file":"x.go","line":10,"body":"fix this","state":"open","at":"2026-01-01T00:00:00Z"}]}
	}`
	writeStateJSON(t, jsonPath, raw)

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.(*sqliteStore).Close()

	rev, err := s.ReviewedFiles("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rev) != 0 {
		t.Errorf("v0.1 review marks are unmappable and must drop, got %+v", rev)
	}

	cs, err := s.Comments("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Body != "fix this" {
		t.Fatalf("comments must never be dropped, got %+v", cs)
	}
	if cs[0].ID != "" || cs[0].Side != "" || cs[0].FileHash != "" {
		t.Errorf("pre-P4 fields should import as zero values, got %+v", cs[0])
	}

	if _, err := os.Stat(jsonPath + ".imported"); err != nil {
		t.Errorf(".imported backup missing after a successful (if partial) import: %v", err)
	}
}

// ---- corrupt db recovery (§4.3) ----

// TestOpenMovesAsideCorruptDBAndStartsFresh pins §4.3: an existing file at
// the SQLite path that fails to open (WP1's OpenSQLite already proves a
// non-database file errors there, TestSQLiteCorruptFileReturnsOpenError)
// gets moved aside to a timestamped backup by store.Open, and a fresh,
// usable db takes its place — stronger than the pre-v0.6 JSON behavior of
// silently zeroing corrupt state.
func TestOpenMovesAsideCorruptDBAndStartsFresh(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	if err := os.WriteFile(dbPath, []byte("not a sqlite database, just garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("a corrupt db must be moved aside and replaced, not fail Open: %v", err)
	}
	defer s.(*sqliteStore).Close()

	if err := s.SetReviewed("wt1", "a.go", "h1"); err != nil {
		t.Fatalf("fresh replacement db should be fully usable: %v", err)
	}

	matches, _ := filepath.Glob(dbPath + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("expected exactly one moved-aside backup, got %v", matches)
	}
	orig, err := os.ReadFile(matches[0])
	if err != nil || string(orig) != "not a sqlite database, just garbage" {
		t.Errorf("moved-aside backup should hold the original corrupt bytes, got %q, err=%v", orig, err)
	}
}

// TestOpenImportsSiblingJSONAfterMovingAsideCorruptDB proves the move-aside
// path (§4.3) and the import trigger (§4.2) compose correctly: once the
// corrupt db is out of the way, the file that replaces it is — from
// store.Open's point of view — an ordinary db-being-created, so an
// untouched sibling state.json is still imported exactly as on a first-ever
// run.
func TestOpenImportsSiblingJSONAfterMovingAsideCorruptDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	jsonPath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(dbPath, []byte("garbage, not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, jsonPath, `{"reviewed_files":{"wt1":{"a.go":"h1"}},"comments":{}}`)

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.(*sqliteStore).Close()

	rev, err := s.ReviewedFiles("wt1")
	if err != nil || rev["a.go"] != "h1" {
		t.Errorf("sibling json should be imported into the fresh replacement db, got %+v, err=%v", rev, err)
	}
	if _, err := os.Stat(jsonPath + ".imported"); err != nil {
		t.Errorf(".imported backup missing: %v", err)
	}
}

// TestMoveAsideCorruptDBUsesUniqueBackupNamesEvenWithinTheSameSecond pins
// the reviewer NIT fix (P6-fixes.md): moveAsideCorruptDB's backup suffix is
// UnixNano, not Unix, so two recoveries of the same path within the same
// wall-clock second get distinct backup files rather than the second
// silently overwriting the first's forensic copy. Two back-to-back calls
// in a fast test are exactly the scenario that used to collide under
// second-resolution timestamps.
func TestMoveAsideCorruptDBUsesUniqueBackupNamesEvenWithinTheSameSecond(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	if err := os.WriteFile(path, []byte("corrupt-1"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup1, err := moveAsideCorruptDB(path, fmt.Errorf("boom"))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("corrupt-2"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup2, err := moveAsideCorruptDB(path, fmt.Errorf("boom again"))
	if err != nil {
		t.Fatal(err)
	}

	if backup1 == backup2 {
		t.Fatalf("two recoveries within the same second produced the SAME backup name %q — the second overwrote the first's forensic copy", backup1)
	}
	b1, err := os.ReadFile(backup1)
	if err != nil || string(b1) != "corrupt-1" {
		t.Errorf("backup1 = %q, err=%v, want %q intact", b1, err, "corrupt-1")
	}
	b2, err := os.ReadFile(backup2)
	if err != nil || string(b2) != "corrupt-2" {
		t.Errorf("backup2 = %q, err=%v, want %q intact", b2, err, "corrupt-2")
	}
}

// ---- import-path comment id collision (DEFECT D1 fix, P6-fixes.md) ----

// TestOpenImportWithDuplicateSourceCommentIDsKeepsFirstAndLogsCount pins
// the import-path half of the D1/D2 fix: a hand-edited or hand-merged
// state.json that itself carries two comments sharing an id must import
// exactly ONE of them (the first encountered), not error out the whole
// import and not (pre-fix) land both as separate rows — and the discarded
// duplicate is logged at WARN with a count, never silent.
func TestOpenImportWithDuplicateSourceCommentIDsKeepsFirstAndLogsCount(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "state.json")
	dbPath := filepath.Join(dir, "state.db")
	raw := `{"reviewed_files":{},"comments":{"wt1":[
		{"id":"c-dup","worktreeId":"wt1","file":"a.go","body":"first","state":"open","at":"2026-01-01T00:00:00Z"},
		{"id":"c-dup","worktreeId":"wt1","file":"a.go","body":"second","state":"open","at":"2026-01-01T00:00:01Z"},
		{"id":"c-other","worktreeId":"wt1","file":"b.go","body":"unrelated","state":"open","at":"2026-01-01T00:00:02Z"}
	]}}`
	writeStateJSON(t, jsonPath, raw)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("a duplicate comment id within the source must not fail the whole import, got: %v", err)
	}
	defer s.(*sqliteStore).Close()

	cs, err := s.Comments("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("comments after import = %+v, want exactly 2 (the duplicate id collapses to its first occurrence, plus c-other)", cs)
	}
	if commentWithBody(cs, "first") == nil {
		t.Errorf("the FIRST occurrence of the duplicate id must survive import, got %+v", cs)
	}
	if commentWithBody(cs, "second") != nil {
		t.Errorf("the second occurrence sharing the id must be discarded, not imported as a separate row: %+v", cs)
	}
	if commentWithBody(cs, "unrelated") == nil {
		t.Errorf("an unrelated comment must still import normally: %+v", cs)
	}

	var sawWarning bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["level"] == "WARN" && strings.Contains(fmt.Sprint(rec["msg"]), "duplicate comment id") {
			sawWarning = true
		}
	}
	if !sawWarning {
		t.Errorf("a source-side duplicate comment id must be logged at WARN with a count, got log output:\n%s", logs.String())
	}
}
