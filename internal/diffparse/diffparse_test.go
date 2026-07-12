package diffparse

import (
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
