package guardrail

import (
	"strings"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/diffparse"
	"github.com/navbytes/wt-cockpit/internal/model"
)

// fakeAWSKeyID is a syntactically AWS-access-key-ID-shaped string ("AKIA" +
// 16 uppercase-alnum chars) reused across this package's tests as a
// stand-in secret. It's built from two literal fragments joined with "+" so
// the 20-byte secret-shaped run of bytes never appears contiguous in this
// source file — GitHub's push-protection secret scanner matches file text,
// not the constant Go folds this into, so the split defeats the scanner
// while every test still sees the exact same value as before.
const fakeAWSKeyID = "AKIA" + "ABCDEFGHIJKLMNOP"

// mustCompile is the test-only equivalent of the old can't-fail New: most
// tests don't care about Compile's error path (that's exercised explicitly
// by the validation-matrix tests below), so this keeps every other test's
// setup a one-liner.
func mustCompile(t testing.TB, rules []Rule) *Engine {
	t.Helper()
	e, err := Compile(rules)
	if err != nil {
		t.Fatalf("Compile(%+v): %v", rules, err)
	}
	return e
}

func sampleDiff() model.Diff {
	return model.Diff{
		WorktreeID: "wt1",
		Files: []model.DiffFile{
			{Path: "migrations/014_drop.sql", Status: model.FileModified, Stats: model.Stats{Add: 2, Del: 96}},
			{Path: "src/app.ts", Status: model.FileModified, Stats: model.Stats{Add: 40, Del: 4}},
			{Path: ".github/workflows/ci.yml", Status: model.FileModified, Stats: model.Stats{Add: 1, Del: 1}},
		},
	}
}

// addedFile builds a DiffFile whose one hunk carries lines of added content —
// the shape content conditions (added_pattern, entropy) scan.
func addedFile(path string, addedLines ...string) model.DiffFile {
	var lines []model.Line
	for i, s := range addedLines {
		lines = append(lines, model.Line{Kind: model.LineAdd, NewNum: i + 1, Content: s})
	}
	return model.DiffFile{
		Path:   path,
		Status: model.FileAdded,
		Stats:  model.Stats{Add: len(addedLines)},
		Hunks:  []model.Hunk{{Header: "@@ -0,0 +1," + itoa(len(addedLines)) + " @@", Lines: lines}},
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// ---- existing v0.2 behaviour, ported to Compile ----

func TestPathGlobRuleTrips(t *testing.T) {
	e := mustCompile(t, []Rule{
		{Name: "touches-migrations", Severity: "danger", PathGlob: "migrations/**", Message: "touches migrations"},
	})
	hits := e.Eval(sampleDiff())
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d: %+v", len(hits), hits)
	}
	if hits[0].Rule != "touches-migrations" || hits[0].File != "migrations/014_drop.sql" {
		t.Errorf("unexpected hit: %+v", hits[0])
	}
	if hits[0].Severity != "danger" {
		t.Errorf("severity = %q", hits[0].Severity)
	}
}

func TestGlobMatchesNestedAndFlat(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "ci", Severity: "warn", PathGlob: ".github/workflows/*", Message: "edits CI"}})
	hits := e.Eval(sampleDiff())
	if len(hits) != 1 || hits[0].File != ".github/workflows/ci.yml" {
		t.Fatalf("glob match failed: %+v", hits)
	}
}

func TestNetDeletionThresholdPerFile(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "big-delete", Severity: "warn", MinNetDeleted: 50, Message: "large deletion"}})
	hits := e.Eval(sampleDiff())
	if len(hits) != 1 || hits[0].File != "migrations/014_drop.sql" {
		t.Fatalf("want big-delete on migrations file, got %+v", hits)
	}
}

func TestDeletionRatioWorktreeWide(t *testing.T) {
	// sample totals: add=43, del=101 -> ratio ~2.35 -> trips.
	e := mustCompile(t, []Rule{{Name: "net-negative", Severity: "warn", MinDeleteAddRatio: 2.0, Message: "deletes >2x additions"}})
	hits := e.Eval(sampleDiff())
	found := false
	for _, h := range hits {
		if h.Rule == "net-negative" && h.File == "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected worktree-wide net-negative hit, got %+v", hits)
	}
}

func TestNoFalsePositives(t *testing.T) {
	clean := model.Diff{Files: []model.DiffFile{
		{Path: "src/util.go", Stats: model.Stats{Add: 5, Del: 1}},
	}}
	e := mustCompile(t, []Rule{
		{Name: "touches-migrations", Severity: "danger", PathGlob: "migrations/**"},
		{Name: "big-delete", Severity: "warn", MinNetDeleted: 50},
		{Name: "net-negative", Severity: "warn", MinDeleteAddRatio: 2.0},
	})
	if hits := e.Eval(clean); len(hits) != 0 {
		t.Fatalf("clean diff should trip nothing, got %+v", hits)
	}
}

// TestRuleWithNoConditionsNeverMatches covers a rule loaded from a config
// [[rules]] table with only "name" set: hasFileCond's guard means such a rule
// matches no file, and it's not a worktree-wide rule either, so it must
// produce zero hits — never a crash, never a false positive on every file.
func TestRuleWithNoConditionsNeverMatches(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "only-a-name"}})
	if hits := e.Eval(sampleDiff()); len(hits) != 0 {
		t.Fatalf("a rule with no conditions should never match, got %+v", hits)
	}
}

func TestDefaultRulesLoad(t *testing.T) {
	e := mustCompile(t, DefaultRules())
	hits := e.Eval(sampleDiff())
	if len(hits) == 0 {
		t.Fatal("default rules should catch the migrations + big-delete case")
	}
}

// ---- new v0.5 per-file conditions ----

// TestPathGlobsSingularEquivalence is the glob-list/singular equivalence
// property: for any (glob, path), path_glob=X and path_globs=[X] must agree.
func TestPathGlobsSingularEquivalence(t *testing.T) {
	cases := []struct {
		glob, path string
	}{
		{"migrations/**", "migrations/014_drop.sql"},
		{"migrations/**", "src/app.ts"},
		{".github/workflows/*", ".github/workflows/ci.yml"},
		{"**/*.lock", "sub/dir/yarn.lock"},
		{"go.mod", "go.mod"},
		{"go.mod", "sub/go.mod"},
	}
	diff := sampleDiff()
	for _, c := range cases {
		singular := mustCompile(t, []Rule{{Name: "r", PathGlob: c.glob, Severity: "warn"}})
		list := mustCompile(t, []Rule{{Name: "r", PathGlobs: []string{c.glob}, Severity: "warn"}})

		got1 := ruleMatchesAnyOf(singular.Eval(diff), c.path)
		got2 := ruleMatchesAnyOf(list.Eval(diff), c.path)
		if got1 != got2 {
			t.Errorf("glob %q path %q: path_glob matched=%v, path_globs matched=%v (want equal)", c.glob, c.path, got1, got2)
		}
	}
}

func ruleMatchesAnyOf(hits []model.GuardrailHit, file string) bool {
	for _, h := range hits {
		if h.File == file {
			return true
		}
	}
	return false
}

func TestPathGlobsAnyOf(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "manifests", PathGlobs: []string{"go.mod", "package.json"}}})
	diff := model.Diff{Files: []model.DiffFile{
		{Path: "go.mod", Stats: model.Stats{Add: 1}},
		{Path: "package.json", Stats: model.Stats{Add: 1}},
		{Path: "other.go", Stats: model.Stats{Add: 1}},
	}}
	hits := e.Eval(diff)
	if len(hits) != 2 {
		t.Fatalf("want 2 hits (go.mod, package.json), got %+v", hits)
	}
}

func TestExcludeGlobsExemptsMatchingFile(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "big-files", MinChangedLines: 10, ExcludeGlobs: []string{"*.lock"}}})
	diff := model.Diff{Files: []model.DiffFile{
		{Path: "yarn.lock", Stats: model.Stats{Add: 20}},
		{Path: "app.go", Stats: model.Stats{Add: 20}},
	}}
	hits := e.Eval(diff)
	if len(hits) != 1 || hits[0].File != "app.go" {
		t.Fatalf("want only app.go to trip (yarn.lock exempted), got %+v", hits)
	}
}

// TestExcludeGlobsAloneMatchesNothing: a rule whose ONLY field is
// exclude_globs has no real condition (per hasFileCond) and so must never
// match, exactly like a rule with no fields at all.
func TestExcludeGlobsAloneMatchesNothing(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "r", ExcludeGlobs: []string{"*.lock"}}})
	if hits := e.Eval(sampleDiff()); len(hits) != 0 {
		t.Fatalf("exclude_globs alone should match nothing, got %+v", hits)
	}
}

func TestStatusCondition(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "deletes", Status: "deleted"}})
	diff := model.Diff{Files: []model.DiffFile{
		{Path: "gone.go", Status: model.FileDeleted},
		{Path: "kept.go", Status: model.FileModified},
	}}
	hits := e.Eval(diff)
	if len(hits) != 1 || hits[0].File != "gone.go" {
		t.Fatalf("want only gone.go (status=deleted), got %+v", hits)
	}
}

func TestBinaryCondition(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "bin-added", Binary: true, Status: "added"}})
	diff := model.Diff{Files: []model.DiffFile{
		{Path: "img.png", Status: model.FileAdded, Binary: true},
		{Path: "text.txt", Status: model.FileAdded, Binary: false},
	}}
	hits := e.Eval(diff)
	if len(hits) != 1 || hits[0].File != "img.png" {
		t.Fatalf("want only img.png (binary+added), got %+v", hits)
	}
}

// realRenameAndBinaryDiff is genuine `git diff`-shaped unified diff text (the
// same shape as diffparse_test.go's own renamedBinary fixture) — a pure
// rename (no content change) and a binary-file modification, side by side.
// Used so the status="renamed"/binary=true conditions are proven against a
// real diffparse.Parse() output, not a hand-built model.DiffFile literal.
const realRenameAndBinaryDiff = `diff --git a/old_name.go b/new_name.go
similarity index 100%
rename from old_name.go
rename to new_name.go
diff --git a/asset.bin b/asset.bin
index 5555555..6666666 100644
Binary files a/asset.bin and b/asset.bin differ
`

// TestStatusRenamedAndBinaryConditionsAgainstRealDiffparseOutput closes two
// PROBE gaps at once: status="renamed" matched against a genuine rename diff,
// and binary=true matched against a genuine binary-file diff — both parsed by
// the real diffparse.Parse, not synthesized model.DiffFile values.
func TestStatusRenamedAndBinaryConditionsAgainstRealDiffparseOutput(t *testing.T) {
	files := diffparse.Parse(realRenameAndBinaryDiff)
	if len(files) != 2 {
		t.Fatalf("precondition: diffparse should produce 2 files, got %d: %+v", len(files), files)
	}
	diff := model.Diff{Files: files}

	e := mustCompile(t, []Rule{
		{Name: "renamed-files", Status: "renamed"},
		{Name: "binary-files", Binary: true},
	})
	hits := e.Eval(diff)

	var sawRename, sawBinary bool
	for _, h := range hits {
		if h.Rule == "renamed-files" && h.File == "new_name.go" {
			sawRename = true
		}
		if h.Rule == "binary-files" && h.File == "asset.bin" {
			sawBinary = true
		}
	}
	if !sawRename {
		t.Errorf("status=renamed should trip on the real renamed file (new_name.go), got %+v", hits)
	}
	if !sawBinary {
		t.Errorf("binary=true should trip on the real binary file (asset.bin), got %+v", hits)
	}
}

func TestMinChangedLinesCondition(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "churn", MinChangedLines: 100}})
	diff := model.Diff{Files: []model.DiffFile{
		{Path: "small.go", Stats: model.Stats{Add: 10, Del: 10}}, // 20 total
		{Path: "large.go", Stats: model.Stats{Add: 60, Del: 60}}, // 120 total
	}}
	hits := e.Eval(diff)
	if len(hits) != 1 || hits[0].File != "large.go" {
		t.Fatalf("want only large.go (>=100 changed lines), got %+v", hits)
	}
}

// TestMinChangedLinesBoundaryExactlyN pins the boundary itself (99 vs 100),
// unlike TestMinChangedLinesCondition's 20-vs-120 example: one line short of
// the threshold must not trip, and exactly N (the ">=" boundary) must.
func TestMinChangedLinesBoundaryExactlyN(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "churn", MinChangedLines: 100}})
	oneUnder := model.Diff{Files: []model.DiffFile{{Path: "a.go", Stats: model.Stats{Add: 50, Del: 49}}}} // 99
	exactlyN := model.Diff{Files: []model.DiffFile{{Path: "b.go", Stats: model.Stats{Add: 50, Del: 50}}}} // 100
	if hits := e.Eval(oneUnder); len(hits) != 0 {
		t.Errorf("99 changed lines (one under the threshold) must not trip, got %+v", hits)
	}
	if hits := e.Eval(exactlyN); len(hits) != 1 || hits[0].File != "b.go" {
		t.Errorf("exactly 100 changed lines must trip (>=), got %+v", hits)
	}
}

// ---- new v0.5 worktree-wide conditions ----

func TestMinFilesChangedCondition(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "blast-radius", MinFilesChanged: 3}})
	few := model.Diff{Files: []model.DiffFile{{Path: "a"}, {Path: "b"}}}
	many := model.Diff{Files: []model.DiffFile{{Path: "a"}, {Path: "b"}, {Path: "c"}}}
	if hits := e.Eval(few); len(hits) != 0 {
		t.Errorf("2 files should not trip a >=3 threshold, got %+v", hits)
	}
	if hits := e.Eval(many); len(hits) != 1 || hits[0].File != "" {
		t.Errorf("3 files should trip as a worktree-wide hit (empty File), got %+v", hits)
	}
}

func TestMinTotalChangedCondition(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "huge-churn", MinTotalChanged: 100}})
	small := model.Diff{Files: []model.DiffFile{{Stats: model.Stats{Add: 10, Del: 10}}}}
	big := model.Diff{Files: []model.DiffFile{{Stats: model.Stats{Add: 60, Del: 60}}}}
	if hits := e.Eval(small); len(hits) != 0 {
		t.Errorf("20 total changed lines should not trip a >=100 threshold, got %+v", hits)
	}
	if hits := e.Eval(big); len(hits) != 1 {
		t.Errorf("120 total changed lines should trip, got %+v", hits)
	}
}

// TestMinTotalChangedBoundaryExactlyN pins the exact boundary (99 vs 100),
// unlike TestMinTotalChangedCondition's 20-vs-120 example.
func TestMinTotalChangedBoundaryExactlyN(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "huge-churn", MinTotalChanged: 100}})
	oneUnder := model.Diff{Files: []model.DiffFile{{Stats: model.Stats{Add: 50, Del: 49}}}} // 99
	exactlyN := model.Diff{Files: []model.DiffFile{{Stats: model.Stats{Add: 50, Del: 50}}}} // 100
	if hits := e.Eval(oneUnder); len(hits) != 0 {
		t.Errorf("99 total changed lines (one under the threshold) must not trip, got %+v", hits)
	}
	if hits := e.Eval(exactlyN); len(hits) != 1 {
		t.Errorf("exactly 100 total changed lines must trip (>=), got %+v", hits)
	}
}

// ---- content conditions: added_pattern, entropy ----

func TestAddedPatternTripsOnAddedLineOnly(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "aws-key", Severity: "danger", AddedPattern: `AKIA[0-9A-Z]{16}`}})
	diff := model.Diff{Files: []model.DiffFile{
		addedFile("config.go", `key := "`+fakeAWSKeyID+`"`, "other line"),
	}}
	hits := e.Eval(diff)
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %+v", hits)
	}
	if hits[0].Line != 1 {
		t.Errorf("Line = %d, want 1 (the matching added line)", hits[0].Line)
	}
}

func TestAddedPatternIgnoresContextAndDeletedLines(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "aws-key", AddedPattern: `AKIA[0-9A-Z]{16}`}})
	f := model.DiffFile{
		Path: "config.go", Status: model.FileModified,
		Hunks: []model.Hunk{{Lines: []model.Line{
			{Kind: model.LineContext, Content: `key := "` + fakeAWSKeyID + `"`},
			{Kind: model.LineDel, Content: `key := "` + fakeAWSKeyID + `"`},
			{Kind: model.LineAdd, NewNum: 1, Content: "unrelated"},
		}}},
	}
	if hits := e.Eval(model.Diff{Files: []model.DiffFile{f}}); len(hits) != 0 {
		t.Fatalf("a pattern match on a context/deleted line must not trip, got %+v", hits)
	}
}

func TestAddedPatternSkipsBinaryFiles(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "aws-key", AddedPattern: `AKIA[0-9A-Z]{16}`}})
	f := addedFile("blob.bin", fakeAWSKeyID)
	f.Binary = true
	if hits := e.Eval(model.Diff{Files: []model.DiffFile{f}}); len(hits) != 0 {
		t.Fatalf("a binary file must never be content-scanned, got %+v", hits)
	}
}

// TestAddedPatternOneHitPerFileWithMatchCount: multiple matching lines in one
// file still produce exactly one hit, and its default message carries a
// match count.
func TestAddedPatternOneHitPerFileWithMatchCount(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "aws-key", AddedPattern: `AKIA[0-9A-Z]{16}`}})
	diff := model.Diff{Files: []model.DiffFile{
		addedFile("config.go",
			`a := "`+fakeAWSKeyID+`"`,
			"unrelated",
			`b := "AKIA`+`ZZZZZZZZZZZZZZZZ"`,
			`c := "AKIA`+`YYYYYYYYYYYYYYYY"`,
		),
	}}
	hits := e.Eval(diff)
	if len(hits) != 1 {
		t.Fatalf("want exactly 1 hit even with 3 matching lines, got %d: %+v", len(hits), hits)
	}
	if hits[0].Line != 1 {
		t.Errorf("Line = %d, want 1 (first match)", hits[0].Line)
	}
	if !strings.Contains(hits[0].Message, "3") {
		t.Errorf("default message = %q, want it to carry the match count (3)", hits[0].Message)
	}
}

func TestEntropyTripsOnHighEntropyToken(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "secret", MinTokenEntropy: 4.8, MinTokenLen: 32}})
	highEntropy := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef" // 32 distinct chars, H=5.0
	diff := model.Diff{Files: []model.DiffFile{addedFile("app.go", `token := "`+highEntropy+`"`)}}
	if hits := e.Eval(diff); len(hits) != 1 {
		t.Fatalf("want the high-entropy token to trip, got %+v", hits)
	}
}

// TestEntropyImmuneToHexBelowThreshold is the shipped rationale: a git SHA /
// checksum is hex-only (entropy <= 4.0) and must never trip a 4.8 threshold,
// however long.
func TestEntropyImmuneToHexBelowThreshold(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "secret", MinTokenEntropy: 4.8, MinTokenLen: 32}})
	sha := "5f4dcc3b5aa765d61d8327deb882cf99" + "5f4dcc3b" // 40-char hex-shaped token
	diff := model.Diff{Files: []model.DiffFile{addedFile("app.go", `commit := "`+sha+`"`)}}
	if hits := e.Eval(diff); len(hits) != 0 {
		t.Fatalf("a hex-only token must never trip the entropy rule, got %+v", hits)
	}
}

func TestEntropyIgnoresTokensShorterThanMinLen(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "secret", MinTokenEntropy: 1.0, MinTokenLen: 32}})
	diff := model.Diff{Files: []model.DiffFile{addedFile("app.go", `x := "Ab3F"`)}} // high per-char entropy, but only 4 chars long
	if hits := e.Eval(diff); len(hits) != 0 {
		t.Fatalf("a token shorter than min_token_len must never trip, got %+v", hits)
	}
}

// TestEntropyExcludeGlobsExemptsLockfiles pins the shipped secrets-entropy
// default's own exclude_globs: the exact same high-entropy token that trips
// in a regular file must be inert inside an excluded path.
func TestEntropyExcludeGlobsExemptsLockfiles(t *testing.T) {
	e := mustCompile(t, DefaultRules())
	highEntropy := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef"
	diff := model.Diff{Files: []model.DiffFile{
		addedFile("go.sum", `h1:`+highEntropy),
		addedFile("app.go", `token := "`+highEntropy+`"`),
	}}
	hits := e.Eval(diff)
	var sawGoSum, sawAppGo bool
	for _, h := range hits {
		if h.Rule != "secrets-entropy" {
			continue
		}
		if h.File == "go.sum" {
			sawGoSum = true
		}
		if h.File == "app.go" {
			sawAppGo = true
		}
	}
	if sawGoSum {
		t.Error("go.sum should be exempted by exclude_globs, but secrets-entropy fired on it")
	}
	if !sawAppGo {
		t.Error("app.go should trip secrets-entropy on the same high-entropy token")
	}
}

// TestContentScanCapsAt5000AddedLines: a pattern that would only match past
// the 5,000-added-line cap must not trip.
func TestContentScanCapsAt5000AddedLines(t *testing.T) {
	e := mustCompile(t, []Rule{{Name: "needle", AddedPattern: `NEEDLE`}})

	lines := make([]string, 5010)
	for i := range lines {
		lines[i] = "filler"
	}
	lines[5005] = "NEEDLE" // past the 5,000-line cap (0-indexed: line 5006)
	diff := model.Diff{Files: []model.DiffFile{addedFile("big.go", lines...)}}
	if hits := e.Eval(diff); len(hits) != 0 {
		t.Fatalf("a match past the 5,000-added-line cap must not trip, got %+v", hits)
	}

	lines[100] = "NEEDLE" // well within the cap
	diff2 := model.Diff{Files: []model.DiffFile{addedFile("big.go", lines...)}}
	if hits := e.Eval(diff2); len(hits) != 1 {
		t.Fatalf("a match within the 5,000-added-line cap should trip, got %+v", hits)
	}
}

// ---- THE NON-ECHO INVARIANT ----

// TestNonEchoInvariantContentRulesNeverLeakMatchedText is the security-
// critical pin (P5-design.md §1.1): a hit's Message must never contain the
// matched token or line content, whether the rule uses its own custom
// Message or the engine's generated default.
func TestNonEchoInvariantContentRulesNeverLeakMatchedText(t *testing.T) {
	const fakeAWSKey = "AKIA" + "ABCDEFGHIJKLMNOPQRST"
	const highEntropySecret = "Zx9qP2mK7wR4tB8vN1cL6hJ3"

	rules := []Rule{
		{Name: "aws-key-custom-msg", Severity: "danger", AddedPattern: `AKIA[0-9A-Z]{16,}`, Message: "secrets-shaped string (known token pattern)"},
		{Name: "aws-key-default-msg", Severity: "danger", AddedPattern: `AKIA[0-9A-Z]{16,}`}, // no Message: exercises the fallback
		{Name: "entropy-custom-msg", MinTokenEntropy: 4.5, MinTokenLen: 20, Message: "high-entropy string — possible secret"},
		{Name: "entropy-default-msg", MinTokenEntropy: 4.5, MinTokenLen: 20}, // no Message: exercises the fallback
	}
	e := mustCompile(t, rules)
	diff := model.Diff{Files: []model.DiffFile{
		addedFile("secrets.go",
			`awsKey := "`+fakeAWSKey+`"`,
			`token := "`+highEntropySecret+`"`,
		),
	}}
	hits := e.Eval(diff)
	if len(hits) == 0 {
		t.Fatal("precondition: the planted secrets should trip at least one rule")
	}
	for _, h := range hits {
		if strings.Contains(h.Message, fakeAWSKey) {
			t.Errorf("rule %q: Message leaks the matched AWS-key-shaped string: %q", h.Rule, h.Message)
		}
		if strings.Contains(h.Message, highEntropySecret) {
			t.Errorf("rule %q: Message leaks the matched high-entropy token: %q", h.Rule, h.Message)
		}
		// Belt and braces: nothing in the hit's own fields carries the raw
		// diff line at all (File/Rule/Severity are metadata; Message is the
		// only free-text field).
		full := h.Rule + h.Severity + h.Message + h.File
		if strings.Contains(full, fakeAWSKey) || strings.Contains(full, highEntropySecret) {
			t.Errorf("some field of hit %+v leaks matched content", h)
		}
	}
}

// TestNonEchoInvariantAcrossFullDefaultPack runs the shipped DefaultRules()
// (not a hand-picked subset) over a torture fixture planting both a known
// AWS-key-shaped token and a random high-entropy string, and asserts no hit's
// Message contains either secret. This is the "secrets torture fixture"
// scenario named in the phase brief.
func TestNonEchoInvariantAcrossFullDefaultPack(t *testing.T) {
	const fakeAWSKey = "AKIA" + "IOSFODNN7EXAMPLE"
	const randomSecret = "qT7xM2vK9pL4nR8wZ3cH6jF1sD5b"

	e := mustCompile(t, DefaultRules())
	diff := model.Diff{Files: []model.DiffFile{
		addedFile("internal/config/secrets.go",
			`const awsAccessKey = "`+fakeAWSKey+`"`,
			`const apiToken = "`+randomSecret+`"`,
		),
	}}
	hits := e.Eval(diff)
	if len(hits) == 0 {
		t.Fatal("precondition: the torture fixture should trip at least one default rule")
	}
	for _, h := range hits {
		if strings.Contains(h.Message, fakeAWSKey) || strings.Contains(h.Message, randomSecret) {
			t.Errorf("default rule %q leaked matched secret content in its message: %q", h.Rule, h.Message)
		}
	}
}

// TestSecretsPatternDoesNotFalsePositiveOnKebabCaseSK is the MAJOR fix pin
// (reviewer M2): the shipped secrets-pattern's sk- branch used to be
// `sk-[A-Za-z0-9_-]{20,}`, so any kebab-case identifier containing the
// literal substring "sk-" followed by 20+ more word/hyphen characters —
// "risk-management-dashboard", "disk-usage-monitoring-service",
// "kiosk-management-system-portal" among them — tripped danger
// (banner+notify+bell fatigue) despite carrying no secret at all. Dropping
// '-'/'_' from the character class (`sk-[A-Za-z0-9]{20,}`) breaks the false
// match at the first hyphen while a real OpenAI-shaped key (sk- plus 20+
// plain alnum, no hyphens) still trips.
func TestSecretsPatternDoesNotFalsePositiveOnKebabCaseSK(t *testing.T) {
	e := mustCompile(t, DefaultRules())
	falsePositives := []string{
		"risk-management-dashboard",
		"disk-usage-monitoring-service",
		"kiosk-management-system-portal",
	}
	for _, s := range falsePositives {
		diff := model.Diff{Files: []model.DiffFile{addedFile("app.go", `name := "`+s+`"`)}}
		for _, h := range e.Eval(diff) {
			if h.Rule == "secrets-pattern" {
				t.Errorf("%q must not trip secrets-pattern (kebab-case false positive), got %+v", s, h)
			}
		}
	}

	// Real OpenAI keys still trip — the classic form and the newer hyphenated
	// project/service-account forms (`sk-proj-`, `sk-svcacct-`), which the plain
	// `sk-[A-Za-z0-9]{20,}` narrowing dropped (security closure LOW). Fixtures
	// are assembled so no contiguous credential literal sits in source.
	mustTrip := []string{
		"sk-" + strings.Repeat("a", 40),
		"sk-" + "proj-" + strings.Repeat("b", 40),
		"sk-" + "svcacct-" + strings.Repeat("c", 40),
	}
	for _, realKey := range mustTrip {
		diff := model.Diff{Files: []model.DiffFile{addedFile("app.go", `key := "`+realKey+`"`)}}
		var tripped bool
		for _, h := range e.Eval(diff) {
			if h.Rule == "secrets-pattern" {
				tripped = true
			}
		}
		if !tripped {
			t.Errorf("a real OpenAI key %q must trip secrets-pattern, got no hit", realKey)
		}
	}
}

// ---- Compile validation matrix ----

func TestCompileRejectsInvalidSeverity(t *testing.T) {
	_, err := Compile([]Rule{{Name: "bad-sev", Severity: "critical", PathGlob: "*"}})
	if err == nil {
		t.Fatal("expected an error for an invalid severity")
	}
	if !strings.Contains(err.Error(), "bad-sev") || !strings.Contains(err.Error(), "critical") {
		t.Errorf("error should name the rule and the bad value, got: %v", err)
	}
}

func TestCompileRejectsDuplicateNames(t *testing.T) {
	_, err := Compile([]Rule{
		{Name: "dup", PathGlob: "a/**"},
		{Name: "dup", PathGlob: "b/**"},
	})
	if err == nil {
		t.Fatal("expected an error for duplicate rule names")
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("error should name the duplicated rule, got: %v", err)
	}
}

func TestCompileAutoFillsBlankNamesWithoutFalseDuplicate(t *testing.T) {
	e, err := Compile([]Rule{{PathGlob: "a/**"}, {PathGlob: "b/**"}})
	if err != nil {
		t.Fatalf("two blank-named rules should auto-fill to distinct names, got: %v", err)
	}
	names := e.Rules()
	if names[0].Name == "" || names[1].Name == "" || names[0].Name == names[1].Name {
		t.Errorf("expected distinct auto-filled names, got %q and %q", names[0].Name, names[1].Name)
	}
}

func TestCompileRejectsPathGlobAndPathGlobsTogether(t *testing.T) {
	_, err := Compile([]Rule{{Name: "both-globs", PathGlob: "a/**", PathGlobs: []string{"b/**"}}})
	if err == nil {
		t.Fatal("expected an error when both path_glob and path_globs are set")
	}
	if !strings.Contains(err.Error(), "both-globs") {
		t.Errorf("error should name the rule, got: %v", err)
	}
}

func TestCompileRejectsMixedPerFileAndWorktreeWideConditions(t *testing.T) {
	_, err := Compile([]Rule{{Name: "mixed", PathGlob: "a/**", MinFilesChanged: 10}})
	if err == nil {
		t.Fatal("expected an error mixing a per-file condition with a worktree-wide condition")
	}
	if !strings.Contains(err.Error(), "mixed") {
		t.Errorf("error should name the rule, got: %v", err)
	}
}

func TestCompileRejectsExcludeGlobsOnWorktreeWideRule(t *testing.T) {
	_, err := Compile([]Rule{{Name: "wt-exclude", MinFilesChanged: 10, ExcludeGlobs: []string{"*.lock"}}})
	if err == nil {
		t.Fatal("expected an error for exclude_globs on a worktree-wide rule")
	}
	if !strings.Contains(err.Error(), "wt-exclude") {
		t.Errorf("error should name the rule, got: %v", err)
	}
}

func TestCompileRejectsEntropyWithoutTokenFloor(t *testing.T) {
	_, err := Compile([]Rule{{Name: "no-floor", MinTokenEntropy: 4.8}})
	if err == nil {
		t.Fatal("expected an error for min_token_entropy without a min_token_len >= 8")
	}
	if !strings.Contains(err.Error(), "no-floor") {
		t.Errorf("error should name the rule, got: %v", err)
	}
}

func TestCompileRejectsEntropyWithTooLowTokenFloor(t *testing.T) {
	_, err := Compile([]Rule{{Name: "low-floor", MinTokenEntropy: 4.8, MinTokenLen: 4}})
	if err == nil {
		t.Fatal("expected an error for min_token_len below 8")
	}
	if !strings.Contains(err.Error(), "low-floor") {
		t.Errorf("error should name the rule, got: %v", err)
	}
}

func TestCompileAcceptsEntropyWithTokenFloorOfExactly8(t *testing.T) {
	if _, err := Compile([]Rule{{Name: "ok-floor", MinTokenEntropy: 4.8, MinTokenLen: 8}}); err != nil {
		t.Errorf("min_token_len == 8 should be accepted, got: %v", err)
	}
}

func TestCompileRejectsNonCompilingAddedPattern(t *testing.T) {
	_, err := Compile([]Rule{{Name: "bad-regex", AddedPattern: "(unterminated["}})
	if err == nil {
		t.Fatal("expected an error for a non-compiling added_pattern")
	}
	if !strings.Contains(err.Error(), "bad-regex") {
		t.Errorf("error should name the rule, got: %v", err)
	}
}

func TestCompileRejectsMultipleWorktreeConditionsMixedWithFileCondition(t *testing.T) {
	// Sanity: MinDeleteAddRatio (the pre-existing worktree-wide field) mixed
	// with a per-file field is rejected the same way the new fields are.
	_, err := Compile([]Rule{{Name: "old-and-new-mixed", MinDeleteAddRatio: 2.0, Status: "deleted"}})
	if err == nil {
		t.Fatal("expected an error mixing min_delete_add_ratio with a per-file condition")
	}
}

// TestCompileAcceptsCombinedWorktreeConditionsOnOneRule: multiple
// worktree-wide fields on the SAME rule are valid (ANDed), not an error.
func TestCompileAcceptsCombinedWorktreeConditionsOnOneRule(t *testing.T) {
	e, err := Compile([]Rule{{Name: "combo", MinFilesChanged: 3, MinTotalChanged: 50}})
	if err != nil {
		t.Fatalf("combining worktree-wide fields on one rule should be valid, got: %v", err)
	}
	trips := model.Diff{Files: []model.DiffFile{
		{Stats: model.Stats{Add: 20}}, {Stats: model.Stats{Add: 20}}, {Stats: model.Stats{Add: 20}},
	}}
	if hits := e.Eval(trips); len(hits) != 1 {
		t.Errorf("3 files totalling 60 changed lines should trip both combined thresholds, got %+v", hits)
	}
	onlyFiles := model.Diff{Files: []model.DiffFile{
		{Stats: model.Stats{Add: 1}}, {Stats: model.Stats{Add: 1}}, {Stats: model.Stats{Add: 1}},
	}}
	if hits := e.Eval(onlyFiles); len(hits) != 0 {
		t.Errorf("3 files but only 3 changed lines should NOT trip the combined total-changed condition, got %+v", hits)
	}
}

// TestDefaultRulesAlwaysCompile guards against ever shipping a DefaultRules()
// entry that would fail its own validation matrix.
func TestDefaultRulesAlwaysCompile(t *testing.T) {
	if _, err := Compile(DefaultRules()); err != nil {
		t.Fatalf("DefaultRules() must always compile cleanly, got: %v", err)
	}
}

// TestCompileRejectsNegativeThresholds is the MINOR-3 fix pin. Every other
// self-contradictory rule shape Compile's own doc comment promises to catch
// ("the same 'typo fails fast' philosophy config.Load already applies") does
// fail fast: bad severity, glob conflicts, condition-class mixing, entropy
// without a token floor, a non-compiling regex. A negative numeric threshold
// (e.g. min_files_changed = -5 — a very plausible typo for 5, or a
// copy-pasted min_net_deleted sign flip) is the same class of nonsensical
// input: every threshold check in hasFileCond/hasWorktreeCond is guarded by
// "> 0", so a negative value reads as indistinguishable from "unset" and the
// rule would otherwise become a permanent, silent no-op — worse than a load
// error, since nothing would tell the rule's author their rule can never fire.
func TestCompileRejectsNegativeThresholds(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
	}{
		{"min_files_changed", Rule{Name: "neg-files", MinFilesChanged: -5}},
		{"min_total_changed", Rule{Name: "neg-total", MinTotalChanged: -100}},
		{"min_changed_lines", Rule{Name: "neg-changed", MinChangedLines: -10}},
		{"min_net_deleted", Rule{Name: "neg-deleted", MinNetDeleted: -50}},
		{"min_delete_add_ratio", Rule{Name: "neg-ratio", MinDeleteAddRatio: -2.0}},
		{"min_token_entropy", Rule{Name: "neg-entropy", MinTokenEntropy: -4.8, MinTokenLen: 32}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Compile([]Rule{c.rule})
			if err == nil {
				t.Fatalf("expected Compile to reject a negative threshold for %+v", c.rule)
			}
			if !strings.Contains(err.Error(), c.rule.Name) {
				t.Errorf("error should name the rule, got: %v", err)
			}
		})
	}
}

// TestCompileIsSelfContainedNoSharedState: compiling the same rules twice
// must not error the second time (i.e. Compile doesn't mutate its input in a
// way that breaks re-use, e.g. from Resolver re-merging per repo).
func TestCompileIsSelfContainedNoSharedState(t *testing.T) {
	rules := DefaultRules()
	if _, err := Compile(rules); err != nil {
		t.Fatal(err)
	}
	if _, err := Compile(rules); err != nil {
		t.Fatalf("compiling the identical rule slice twice should succeed both times, got: %v", err)
	}
}
