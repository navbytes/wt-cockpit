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
