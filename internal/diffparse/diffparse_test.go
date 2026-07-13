package diffparse

import (
	"strings"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

const modifiedDiff = `diff --git a/internal/auth/token.go b/internal/auth/token.go
index 1111111..2222222 100644
--- a/internal/auth/token.go
+++ b/internal/auth/token.go
@@ -18,7 +18,9 @@ func NewToken(uid string) (*Token, error) {
 	exp := time.Now().Add(15 * time.Minute)
-	claims := Claims{Subject: uid}
+	// widened per SEC-412
+	claims := Claims{Subject: uid, Exp: exp}
+	claims.Issuer = "api-server"
 	return sign(claims)
`

func TestParseModifiedFile(t *testing.T) {
	files := Parse(modifiedDiff)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Path != "internal/auth/token.go" {
		t.Errorf("path = %q", f.Path)
	}
	if f.Status != model.FileModified {
		t.Errorf("status = %q, want modified", f.Status)
	}
	if f.Stats.Add != 3 || f.Stats.Del != 1 {
		t.Errorf("stats = +%d -%d, want +3 -1", f.Stats.Add, f.Stats.Del)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(f.Hunks))
	}
	h := f.Hunks[0]
	if h.OldStart != 18 || h.NewStart != 18 {
		t.Errorf("hunk starts old=%d new=%d, want 18/18", h.OldStart, h.NewStart)
	}
	// Verify line numbering: first context line is old 18 / new 18.
	if h.Lines[0].Kind != model.LineContext || h.Lines[0].OldNum != 18 || h.Lines[0].NewNum != 18 {
		t.Errorf("line0 = %+v", h.Lines[0])
	}
	// The deletion should carry an old line number but no new number.
	var del model.Line
	for _, l := range h.Lines {
		if l.Kind == model.LineDel {
			del = l
			break
		}
	}
	if del.OldNum != 19 || del.NewNum != 0 {
		t.Errorf("del line = old %d new %d, want old19 new0", del.OldNum, del.NewNum)
	}
	// An added line should carry a new number but no old number.
	var add model.Line
	for _, l := range h.Lines {
		if l.Kind == model.LineAdd {
			add = l
			break
		}
	}
	if add.NewNum == 0 || add.OldNum != 0 {
		t.Errorf("add line = old %d new %d, want old0 new>0", add.OldNum, add.NewNum)
	}
}

const addedDiff = `diff --git a/src/components/ThemeToggle.tsx b/src/components/ThemeToggle.tsx
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/src/components/ThemeToggle.tsx
@@ -0,0 +1,3 @@
+export function ThemeToggle() {
+  return null
+}
`

func TestParseAddedFile(t *testing.T) {
	files := Parse(addedDiff)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Status != model.FileAdded {
		t.Errorf("status = %q, want added", f.Status)
	}
	if f.Stats.Add != 3 || f.Stats.Del != 0 {
		t.Errorf("stats = +%d -%d, want +3 -0", f.Stats.Add, f.Stats.Del)
	}
	if f.Path != "src/components/ThemeToggle.tsx" {
		t.Errorf("path = %q", f.Path)
	}
}

const deletedDiff = `diff --git a/old/legacy.sql b/old/legacy.sql
deleted file mode 100644
index 4444444..0000000
--- a/old/legacy.sql
+++ /dev/null
@@ -1,2 +0,0 @@
-DROP TABLE users;
-DROP TABLE sessions;
`

func TestParseDeletedFile(t *testing.T) {
	f := Parse(deletedDiff)[0]
	if f.Status != model.FileDeleted {
		t.Errorf("status = %q, want deleted", f.Status)
	}
	if f.Stats.Del != 2 || f.Stats.Add != 0 {
		t.Errorf("stats = +%d -%d, want +0 -2", f.Stats.Add, f.Stats.Del)
	}
	if f.Path != "old/legacy.sql" {
		t.Errorf("path = %q", f.Path)
	}
}

const renamedBinary = `diff --git a/a.png b/b.png
similarity index 100%
rename from a.png
rename to b.png
diff --git a/logo.bin b/logo.bin
index 5555555..6666666 100644
Binary files a/logo.bin and b/logo.bin differ
`

func TestParseRenameAndBinary(t *testing.T) {
	files := Parse(renamedBinary)
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	if files[0].Status != model.FileRenamed || files[0].OldPath != "a.png" || files[0].Path != "b.png" {
		t.Errorf("rename parsed wrong: %+v", files[0])
	}
	if !files[1].Binary {
		t.Errorf("logo.bin should be binary: %+v", files[1])
	}
}

const multiFile = `diff --git a/one.go b/one.go
index aaa..bbb 100644
--- a/one.go
+++ b/one.go
@@ -1,1 +1,2 @@
 package one
+// added
diff --git a/two.go b/two.go
index ccc..ddd 100644
--- a/two.go
+++ b/two.go
@@ -1,2 +1,1 @@
 package two
-// removed
`

func TestParseMultipleFiles(t *testing.T) {
	files := Parse(multiFile)
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	if files[0].Path != "one.go" || files[1].Path != "two.go" {
		t.Errorf("paths = %q, %q", files[0].Path, files[1].Path)
	}
	if files[0].Stats.Add != 1 || files[1].Stats.Del != 1 {
		t.Errorf("stats wrong: %+v %+v", files[0].Stats, files[1].Stats)
	}
}

func TestParseEmpty(t *testing.T) {
	if got := Parse(""); len(got) != 0 {
		t.Errorf("empty diff should yield no files, got %d", len(got))
	}
}

// The fixtures below are captured verbatim (byte-for-byte, via od -c) from
// real `git diff` output (git 2.54.0), not hand-typed — DEFECT D1 is about
// two behaviours only real git output demonstrates together: core.quotePath
// (default true) wraps a path containing a non-ASCII byte in double quotes
// with C-style octal escapes (café.txt -> "caf\303\251.txt"), and,
// completely independently, git always appends a bare trailing tab after a
// "---"/"+++ " path that merely *contains a space*.

const unicodeAddDiff = "diff --git \"a/caf\\303\\251.txt\" \"b/caf\\303\\251.txt\"\n" +
	"new file mode 100644\n" +
	"index 0000000..2ccb1d1\n" +
	"--- /dev/null\n" +
	"+++ \"b/caf\\303\\251.txt\"\n" +
	"@@ -0,0 +1 @@\n" +
	"+hello cafe accent\n"

func TestParseDecodesQuotedOctalEscapedUnicodePath(t *testing.T) {
	files := Parse(unicodeAddDiff)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Path != "café.txt" {
		t.Errorf("path = %q, want %q", f.Path, "café.txt")
	}
	if f.Status != model.FileAdded {
		t.Errorf("status = %q, want added", f.Status)
	}
}

const spacedModifyDiff = "diff --git a/file with space.txt b/file with space.txt\n" +
	"index df967b9..acc0f1b 100644\n" +
	"--- a/file with space.txt\t\n" +
	"+++ b/file with space.txt\t\n" +
	"@@ -1 +1,2 @@\n" +
	" base\n" +
	"+edited\n"

func TestParseTrimsTrailingTabForSpacedPath(t *testing.T) {
	files := Parse(spacedModifyDiff)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Path != "file with space.txt" {
		t.Errorf("path = %q, want %q (no trailing tab)", f.Path, "file with space.txt")
	}
	if f.OldPath != "file with space.txt" {
		t.Errorf(`oldPath = %q, want %q (sourced from the "--- " line, not the ambiguous "diff --git" split)`, f.OldPath, "file with space.txt")
	}
	if f.OldBlob != "df967b9" || f.NewBlob != "acc0f1b" {
		t.Errorf("blobs = %q..%q, want df967b9..acc0f1b", f.OldBlob, f.NewBlob)
	}
}

const spacedUnicodeModifyDiff = "diff --git \"a/space caf\\303\\251.txt\" \"b/space caf\\303\\251.txt\"\n" +
	"index 587be6b..b77b4eb 100644\n" +
	"--- \"a/space caf\\303\\251.txt\"\t\n" +
	"+++ \"b/space caf\\303\\251.txt\"\t\n" +
	"@@ -1 +1,2 @@\n" +
	" x\n" +
	"+y\n"

func TestParseDecodesQuotedPathWithBothSpaceAndUnicodeAndTrailingTab(t *testing.T) {
	files := Parse(spacedUnicodeModifyDiff)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	const want = "space café.txt"
	if f.Path != want {
		t.Errorf("path = %q, want %q", f.Path, want)
	}
	if f.OldPath != want {
		t.Errorf("oldPath = %q, want %q", f.OldPath, want)
	}
}

const deletedSpacedDiff = "diff --git a/new name with space.txt b/new name with space.txt\n" +
	"deleted file mode 100644\n" +
	"index 83db48f..0000000\n" +
	"--- a/new name with space.txt\t\n" +
	"+++ /dev/null\n" +
	"@@ -1,3 +0,0 @@\n" +
	"-line1\n" +
	"-line2\n" +
	"-line3\n"

// TestParseSetsPathFromDashLineForDeletedSpacedFile: for a deleted file,
// model.DiffFile.Path holds the *old* path (there is no "+++" real path to
// set it, per model.go's doc comment) — it must come from "--- ", not the
// ambiguous two-paths-on-one-line "diff --git" split, which mangles it for
// any space-containing name.
func TestParseSetsPathFromDashLineForDeletedSpacedFile(t *testing.T) {
	f := Parse(deletedSpacedDiff)[0]
	if f.Status != model.FileDeleted {
		t.Errorf("status = %q, want deleted", f.Status)
	}
	const want = "new name with space.txt"
	if f.Path != want {
		t.Errorf("path = %q, want %q", f.Path, want)
	}
}

const renamedUnicodeDiff = "diff --git a/plain.txt \"b/ren\\303\\241med.txt\"\n" +
	"similarity index 100%\n" +
	"rename from plain.txt\n" +
	"rename to \"ren\\303\\241med.txt\"\n"

func TestParseDecodesQuotedUnicodeRenameToPath(t *testing.T) {
	f := Parse(renamedUnicodeDiff)[0]
	if f.Status != model.FileRenamed {
		t.Errorf("status = %q, want renamed", f.Status)
	}
	if f.OldPath != "plain.txt" {
		t.Errorf("oldPath = %q, want plain.txt", f.OldPath)
	}
	const want = "renámed.txt"
	if f.Path != want {
		t.Errorf("path = %q, want %q", f.Path, want)
	}
}

const binaryModifyDiff = "diff --git a/a.bin b/a.bin\n" +
	"index a04f2ac..89e1de6 100644\n" +
	"Binary files a/a.bin and b/a.bin differ\n"

// TestParseIndexLineExtractsBlobsForBinaryModify is the parsing half of
// DEFECT/FIX B1: a binary file's diff never has hunks, so OldBlob/NewBlob are
// the only signal left that its content actually changed.
func TestParseIndexLineExtractsBlobsForBinaryModify(t *testing.T) {
	f := Parse(binaryModifyDiff)[0]
	if !f.Binary {
		t.Fatalf("expected binary file: %+v", f)
	}
	if f.OldBlob != "a04f2ac" || f.NewBlob != "89e1de6" {
		t.Errorf("blobs = %q..%q, want a04f2ac..89e1de6", f.OldBlob, f.NewBlob)
	}
	if len(f.Hunks) != 0 {
		t.Errorf("binary file should have zero hunks, got %d", len(f.Hunks))
	}
}

const modeChangeIndexLineHasNoModeSuffixDiff = "diff --git a/file with space.txt b/file with space.txt\n" +
	"old mode 100644\n" +
	"new mode 100755\n" +
	"index df967b9..acc0f1b\n" +
	"--- a/file with space.txt\t\n" +
	"+++ b/file with space.txt\t\n" +
	"@@ -1 +1,2 @@\n" +
	" base\n" +
	"+edited\n"

// TestParseIndexLineWithoutModeSuffixStillExtractsBlobs: git omits the
// trailing mode on the "index" line whenever the file's mode also changed
// (carried instead by separate "old mode"/"new mode" lines) — the blobs must
// still parse.
func TestParseIndexLineWithoutModeSuffixStillExtractsBlobs(t *testing.T) {
	f := Parse(modeChangeIndexLineHasNoModeSuffixDiff)[0]
	if f.OldBlob != "df967b9" || f.NewBlob != "acc0f1b" {
		t.Errorf("blobs = %q..%q, want df967b9..acc0f1b", f.OldBlob, f.NewBlob)
	}
}

const pureRenameNoIndexLineDiff = "diff --git a/a.bin b/b.bin\n" +
	"similarity index 100%\n" +
	"rename from a.bin\n" +
	"rename to b.bin\n"

// TestParseNoIndexLineForPureRenameLeavesBlobsEmpty: git omits the "index"
// line entirely for a pure (100%-similarity) rename — the defensive case B1
// calls out explicitly. Nothing should panic, and the blobs just stay "".
func TestParseNoIndexLineForPureRenameLeavesBlobsEmpty(t *testing.T) {
	f := Parse(pureRenameNoIndexLineDiff)[0]
	if f.Status != model.FileRenamed || f.OldPath != "a.bin" || f.Path != "b.bin" {
		t.Errorf("rename parsed wrong: %+v", f)
	}
	if f.OldBlob != "" || f.NewBlob != "" {
		t.Errorf("a pure (100%%-similarity) rename has no index line; blobs should stay empty, got %q..%q", f.OldBlob, f.NewBlob)
	}
	if len(f.Hunks) != 0 {
		t.Errorf("pure rename should have zero hunks, got %d", len(f.Hunks))
	}
}

// controlByteDiff is DEFECT D2's diffparse-level fixture: a raw ESC (0x1b)
// reaches Parse three independent ways at once — quoted+octal-escaped in a
// path (real git core.quotePath output for a file whose name literally
// contains a control byte, same encoding style as the café.txt fixtures
// above), verbatim in a hunk header's trailing "nearby function" section
// (copied straight from source text), and verbatim in hunk line content
// (added/removed source bytes) — plus a BEL (0x07) in content for good
// measure. None of these are hand-decoded; this is exactly the byte pattern
// real git diff output would contain for a hostile/unusual file.
const controlByteDiff = "diff --git \"a/evil\\033name.go\" \"b/evil\\033name.go\"\n" +
	"index 1111111..2222222 100644\n" +
	"--- \"a/evil\\033name.go\"\n" +
	"+++ \"b/evil\\033name.go\"\n" +
	"@@ -1,1 +1,1 @@ func Evil\x1bTitle(\n" +
	"-old\x1bline\n" +
	"+new\x1bline\x07bell\n"

// TestParseSanitizesControlBytesInContentPathAndHeader is DEFECT D2's
// diffparse unit test: ESC embedded in content, in a quoted path's octal
// escape, and in a hunk header's source-derived section must all come out
// of Parse with zero raw 0x1b (or other control byte) survivors, replaced by
// visible caret notation instead of silently dropped or left verbatim.
// ---- FIX: "-- "/"++ " content lines byte-colliding with "--- "/"+++ " headers ----
//
// The three fixtures below are captured verbatim from real `git diff`
// output (git 2.54.0), same as the D1/D2 fixtures above: a deleted content
// line is prefixed with a single literal '-' marker, and an added line with
// a single literal '+' — so a deleted line whose own text happens to start
// with "-- " (two dashes, a space) comes out byte-identical, for the first
// four columns, to a "--- a/<path>" header line, and symmetrically "++ "
// content collides with a "+++ b/<path>" header. The parser's per-file
// state machine matched "--- "/"+++ " (and neighboring header lines) by text
// alone, with no notion of "are we still in this file's header block, or
// already inside a hunk body" — so it misfired on hunk-body content lines
// that happened to share a header's prefix bytes, corrupting Path/OldPath
// and undercounting Stats.

const deletedFileContentStartsWithDashDashDiff = `diff --git a/old/legacy.sql b/old/legacy.sql
deleted file mode 100644
index ed223db..0000000
--- a/old/legacy.sql
+++ /dev/null
@@ -1,3 +0,0 @@
--- one
---- two
-plain three
`

// TestParseDeletedFileContentStartingWithDashesIsNotMistakenForHeader is the
// core repro: the deleted file's first content line is "-- one" (two
// dashes, a space) — prefixed with git's single '-' delete marker, the raw
// diff line is "--- one", byte-identical to a "--- " old-path header up to
// the 4th column. Before the fix this line was consumed as a header,
// overwriting Path/OldPath with the comment text and dropping it from
// Stats.Del.
func TestParseDeletedFileContentStartingWithDashesIsNotMistakenForHeader(t *testing.T) {
	files := Parse(deletedFileContentStartsWithDashDashDiff)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Status != model.FileDeleted {
		t.Errorf("status = %q, want deleted", f.Status)
	}
	if f.Path != "old/legacy.sql" {
		t.Errorf("path = %q, want old/legacy.sql (must survive a hunk-body line that looks like a header)", f.Path)
	}
	if f.OldPath != "old/legacy.sql" {
		t.Errorf("oldPath = %q, want old/legacy.sql", f.OldPath)
	}
	if f.OldBlob != "ed223db" || f.NewBlob != "0000000" {
		t.Errorf("blobs = %q..%q, want ed223db..0000000", f.OldBlob, f.NewBlob)
	}
	if f.Stats.Del != 3 || f.Stats.Add != 0 {
		t.Errorf("stats = +%d -%d, want +0 -3 (every content line counted, none stolen by a false header match)", f.Stats.Add, f.Stats.Del)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(f.Hunks))
	}
	h := f.Hunks[0]
	if len(h.Lines) != 3 {
		t.Fatalf("want 3 hunk lines, got %d: %+v", len(h.Lines), h.Lines)
	}
	wantContent := []string{"-- one", "--- two", "plain three"}
	for i, want := range wantContent {
		l := h.Lines[i]
		if l.Kind != model.LineDel {
			t.Errorf("line %d kind = %q, want del", i, l.Kind)
		}
		if l.Content != want {
			t.Errorf("line %d content = %q, want %q", i, l.Content, want)
		}
		if l.OldNum != i+1 {
			t.Errorf("line %d oldNum = %d, want %d", i, l.OldNum, i+1)
		}
	}
}

const addedFileContentStartsWithPlusPlusDiff = `diff --git a/src/plus.txt b/src/plus.txt
new file mode 100644
index 0000000..6f5336a
--- /dev/null
+++ b/src/plus.txt
@@ -0,0 +1,3 @@
+++ existing
+++ prefix line
+plain
`

// TestParseAddedFileContentStartingWithPlusesIsNotMistakenForHeader mirrors
// the deleted-file repro above for the symmetric "+++ " collision on an
// added file's content ("++ existing" -> raw line "+++ existing").
func TestParseAddedFileContentStartingWithPlusesIsNotMistakenForHeader(t *testing.T) {
	files := Parse(addedFileContentStartsWithPlusPlusDiff)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Status != model.FileAdded {
		t.Errorf("status = %q, want added", f.Status)
	}
	if f.Path != "src/plus.txt" {
		t.Errorf("path = %q, want src/plus.txt (must survive a hunk-body line that looks like a header)", f.Path)
	}
	if f.OldBlob != "0000000" || f.NewBlob != "6f5336a" {
		t.Errorf("blobs = %q..%q, want 0000000..6f5336a", f.OldBlob, f.NewBlob)
	}
	if f.Stats.Add != 3 || f.Stats.Del != 0 {
		t.Errorf("stats = +%d -%d, want +3 -0", f.Stats.Add, f.Stats.Del)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(f.Hunks))
	}
	h := f.Hunks[0]
	if len(h.Lines) != 3 {
		t.Fatalf("want 3 hunk lines, got %d: %+v", len(h.Lines), h.Lines)
	}
	wantContent := []string{"++ existing", "++ prefix line", "plain"}
	for i, want := range wantContent {
		l := h.Lines[i]
		if l.Kind != model.LineAdd {
			t.Errorf("line %d kind = %q, want add", i, l.Kind)
		}
		if l.Content != want {
			t.Errorf("line %d content = %q, want %q", i, l.Content, want)
		}
		if l.NewNum != i+1 {
			t.Errorf("line %d newNum = %d, want %d", i, l.NewNum, i+1)
		}
	}
}

const modifiedFileContextLinesStartWithDashesAndPlusesDiff = `diff --git a/mod/f.txt b/mod/f.txt
index e0f6e2b..75a36b7 100644
--- a/mod/f.txt
+++ b/mod/f.txt
@@ -1,4 +1,5 @@
 header
 -- ctx dash
 ++ ctx plus
+CHANGED
 footer
`

// TestParseModifiedFileContextLinesStartingWithDashesOrPlusesParseCorrectly
// is the audit's third case: a *context* line (single leading space, not a
// diff marker) whose own text starts with "--"/"++" can never byte-collide
// with a "--- "/"+++ " header — the mandatory leading space sits where the
// header's 3rd/4th dash or plus would need to be — but it's pinned here as a
// regression guard now that the header cases are gated on hunk state, so a
// context line starting the same way stays correctly classified.
func TestParseModifiedFileContextLinesStartingWithDashesOrPlusesParseCorrectly(t *testing.T) {
	f := Parse(modifiedFileContextLinesStartWithDashesAndPlusesDiff)[0]
	if f.Status != model.FileModified {
		t.Errorf("status = %q, want modified", f.Status)
	}
	if f.Path != "mod/f.txt" || f.OldPath != "mod/f.txt" {
		t.Errorf("path/oldPath = %q/%q, want mod/f.txt/mod/f.txt", f.Path, f.OldPath)
	}
	if f.OldBlob != "e0f6e2b" || f.NewBlob != "75a36b7" {
		t.Errorf("blobs = %q..%q, want e0f6e2b..75a36b7", f.OldBlob, f.NewBlob)
	}
	if f.Stats.Add != 1 || f.Stats.Del != 0 {
		t.Errorf("stats = +%d -%d, want +1 -0", f.Stats.Add, f.Stats.Del)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(f.Hunks))
	}
	h := f.Hunks[0]
	if len(h.Lines) != 5 {
		t.Fatalf("want 5 hunk lines, got %d: %+v", len(h.Lines), h.Lines)
	}
	wantKinds := []model.LineKind{model.LineContext, model.LineContext, model.LineContext, model.LineAdd, model.LineContext}
	wantContent := []string{"header", "-- ctx dash", "++ ctx plus", "CHANGED", "footer"}
	for i, want := range wantContent {
		if h.Lines[i].Kind != wantKinds[i] {
			t.Errorf("line %d kind = %q, want %q", i, h.Lines[i].Kind, wantKinds[i])
		}
		if h.Lines[i].Content != want {
			t.Errorf("line %d content = %q, want %q", i, h.Lines[i].Content, want)
		}
	}
}

func TestParseSanitizesControlBytesInContentPathAndHeader(t *testing.T) {
	files := Parse(controlByteDiff)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]

	assertClean := func(label, s string) {
		t.Helper()
		if strings.ContainsRune(s, 0x1b) {
			t.Errorf("%s = %q still contains a raw ESC (0x1b)", label, s)
		}
	}
	assertClean("Path", f.Path)
	assertClean("OldPath", f.OldPath)
	if !strings.Contains(f.Path, "evil^[name.go") {
		t.Errorf("Path = %q, want the decoded ESC visibly escaped as ^[", f.Path)
	}

	if len(f.Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(f.Hunks))
	}
	h := f.Hunks[0]
	assertClean("Hunk.Header", h.Header)
	if !strings.Contains(h.Header, "Evil^[Title(") {
		t.Errorf("Hunk.Header = %q, want the source-derived ESC visibly escaped as ^[", h.Header)
	}

	if len(h.Lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(h.Lines))
	}
	for _, ln := range h.Lines {
		assertClean("Line.Content", ln.Content)
		if strings.ContainsRune(ln.Content, 0x07) {
			t.Errorf("Line.Content = %q still contains a raw BEL (0x07)", ln.Content)
		}
	}
	if !strings.Contains(h.Lines[0].Content, "old^[line") {
		t.Errorf("Lines[0].Content = %q, want old^[line", h.Lines[0].Content)
	}
	if !strings.Contains(h.Lines[1].Content, "new^[line^Gbell") {
		t.Errorf("Lines[1].Content = %q, want new^[line^Gbell", h.Lines[1].Content)
	}
}
