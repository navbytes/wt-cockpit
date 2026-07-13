package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/navbytes/wt-cockpit/internal/guardrail"
)

func writeTOML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const fullExample = `
roots = ["/home/nav/code", "/home/nav/work"]
base = "main"
socket = "/home/nav/.wtcockpit/wtd.sock"
tcp = "127.0.0.1:7799"
web = "127.0.0.1:7788"
state = "/home/nav/.wtcockpit/state.json"
interval = "5s"
watch = "fsnotify"
log_format = "json"
log_level = "debug"

[repos."/home/nav/code/api-server"]
base = "develop"

[[rules]]
name = "touches-migrations"
severity = "danger"
path_glob = "**/migrations/**"
message = "touches database migrations"

[[rules]]
name = "large-deletion"
severity = "warn"
min_net_deleted = 80

[notifications]
enabled = true
severity = "warn"
cooldown = "5m"
`

func TestLoadFullExamplePopulatesEveryField(t *testing.T) {
	path := writeTOML(t, fullExample)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	wantRoots := []string{"/home/nav/code", "/home/nav/work"}
	if len(cfg.Roots) != len(wantRoots) || cfg.Roots[0] != wantRoots[0] || cfg.Roots[1] != wantRoots[1] {
		t.Errorf("Roots = %v, want %v", cfg.Roots, wantRoots)
	}
	if cfg.Base != "main" {
		t.Errorf("Base = %q, want main", cfg.Base)
	}
	if cfg.Socket != "/home/nav/.wtcockpit/wtd.sock" {
		t.Errorf("Socket = %q", cfg.Socket)
	}
	if cfg.TCP != "127.0.0.1:7799" {
		t.Errorf("TCP = %q", cfg.TCP)
	}
	if cfg.Web != "127.0.0.1:7788" {
		t.Errorf("Web = %q, want 127.0.0.1:7788", cfg.Web)
	}
	if cfg.State != "/home/nav/.wtcockpit/state.json" {
		t.Errorf("State = %q", cfg.State)
	}
	if cfg.Interval != 5*time.Second {
		t.Errorf("Interval = %v, want 5s", cfg.Interval)
	}
	if cfg.Watch != "fsnotify" {
		t.Errorf("Watch = %q", cfg.Watch)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}

	rc, ok := cfg.Repos["/home/nav/code/api-server"]
	if !ok {
		t.Fatalf("Repos missing api-server override, got %+v", cfg.Repos)
	}
	if rc.Base != "develop" {
		t.Errorf("per-repo base = %q, want develop", rc.Base)
	}

	if !cfg.RulesSet {
		t.Error("RulesSet should be true when [[rules]] is present")
	}
	if len(cfg.Rules) != 2 {
		t.Fatalf("Rules = %+v, want 2 entries", cfg.Rules)
	}
	// guardrail.Rule now carries slice fields (PathGlobs/ExcludeGlobs), so it's
	// no longer comparable via == — reflect.DeepEqual is the v0.5 equivalent.
	want0 := guardrail.Rule{Name: "touches-migrations", Severity: "danger", PathGlob: "**/migrations/**", Message: "touches database migrations"}
	if !reflect.DeepEqual(cfg.Rules[0], want0) {
		t.Errorf("Rules[0] = %+v, want %+v", cfg.Rules[0], want0)
	}
	want1 := guardrail.Rule{Name: "large-deletion", Severity: "warn", MinNetDeleted: 80}
	if !reflect.DeepEqual(cfg.Rules[1], want1) {
		t.Errorf("Rules[1] = %+v, want %+v", cfg.Rules[1], want1)
	}

	if !cfg.Notifications.EnabledOr() {
		t.Error("Notifications.EnabledOr() should be true")
	}
	if got := cfg.Notifications.SeverityOr(); got != "warn" {
		t.Errorf("Notifications.SeverityOr() = %q, want warn", got)
	}
	if got := cfg.Notifications.CooldownOr(); got != 5*time.Minute {
		t.Errorf("Notifications.CooldownOr() = %v, want 5m", got)
	}
}

func TestLoadMissingFileReturnsZeroConfigNoError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist", "config.toml")
	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if !reflect.DeepEqual(cfg, Config{}) {
		t.Errorf("expected zero Config, got %+v", cfg)
	}
}

func TestLoadUnknownKeyErrorsAndNamesIt(t *testing.T) {
	path := writeTOML(t, `bas = "main"`+"\n") // typo of "base"
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an unrecognised key")
	}
	if got := err.Error(); !strings.Contains(got, "bas") {
		t.Errorf("error should name the offending key %q, got: %v", "bas", got)
	}
}

// TestLoadZeroByteFileReturnsZeroConfigNoError: an existing-but-empty file is
// a distinct code path from "missing" (os.IsNotExist never fires; the normal
// toml.DecodeFile branch runs on empty content instead) and must land on the
// exact same observable outcome — zero Config, nil error.
func TestLoadZeroByteFileReturnsZeroConfigNoError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("an existing empty file should not error, got %v", err)
	}
	if !reflect.DeepEqual(cfg, Config{}) {
		t.Errorf("expected zero Config for an empty file, got %+v", cfg)
	}
}

// TestLoadIntervalZeroAndNegativeParseWithoutError documents that Load is a
// dumb parser: it does not reject an interval of "0s" or a negative duration.
// (Downstream, cmd/wtd's mergeSetting treats a zero config value as "unset" —
// falling back to the flag default — while a *negative* value is nonzero and
// passes straight through; the watcher's own `interval <= 0` guard is what
// finally prevents a non-positive time.NewTicker duration. See
// cmd/wtd/main_test.go and internal/watcher/poller_test.go for those pieces.)
func TestLoadIntervalZeroAndNegativeParseWithoutError(t *testing.T) {
	path := writeTOML(t, `interval = "0s"`+"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf(`interval = "0s" should parse cleanly, got %v`, err)
	}
	if cfg.Interval != 0 {
		t.Errorf("Interval = %v, want 0", cfg.Interval)
	}

	path = writeTOML(t, `interval = "-5s"`+"\n")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf(`interval = "-5s" should parse cleanly, got %v`, err)
	}
	if cfg.Interval != -5*time.Second {
		t.Errorf("Interval = %v, want -5s", cfg.Interval)
	}
}

// TestLoadRuleWithOnlyOneFieldSetKeepsOthersZero: a [[rules]] entry that only
// sets "name" must decode with every other field at its TOML zero value
// (not e.g. inheriting anything from DefaultRules), and still count as
// RulesSet — [[rules]] being present at all, however sparse, replaces the
// defaults outright per RulesOr's contract.
func TestLoadRuleWithOnlyOneFieldSetKeepsOthersZero(t *testing.T) {
	path := writeTOML(t, "[[rules]]\nname = \"only-a-name\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RulesSet {
		t.Error("RulesSet should be true when [[rules]] is present, even with one field set")
	}
	if len(cfg.Rules) != 1 {
		t.Fatalf("Rules = %+v, want 1 entry", cfg.Rules)
	}
	want := guardrail.Rule{Name: "only-a-name"}
	if !reflect.DeepEqual(cfg.Rules[0], want) {
		t.Errorf("Rules[0] = %+v, want %+v (every other field zero)", cfg.Rules[0], want)
	}
}

// TestLoadMistypedNestedRuleFieldErrorsAndNamesIt is MN1's [[rules]] sibling
// to TestLoadUnknownKeyErrorsAndNamesIt: a typo inside an array-of-tables
// entry ("pathglob" for "path_glob") is exactly the hand-edited-guardrails-
// file footgun Load's doc comment calls out, and meta.Undecoded() must catch
// it there too, not just at the top level.
func TestLoadMistypedNestedRuleFieldErrorsAndNamesIt(t *testing.T) {
	path := writeTOML(t, "[[rules]]\nname = \"touches-migrations\"\npathglob = \"**/migrations/**\"\n") // typo of "path_glob"
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a mistyped [[rules]] field")
	}
	if got := err.Error(); !strings.Contains(got, "pathglob") {
		t.Errorf("error should name the offending key %q, got: %v", "pathglob", got)
	}
}

// TestLoadRejectsRuleFailingCompileValidation is the v0.5 sibling of
// TestLoadUnknownKeyErrorsAndNamesIt: a [[rules]] entry that parses fine as
// TOML but is self-contradictory (here, an invalid severity) must still fail
// Load with a message naming the offending rule, per guardrail.Compile's
// validation matrix routed through config.Load.
func TestLoadRejectsRuleFailingCompileValidation(t *testing.T) {
	path := writeTOML(t, "[[rules]]\nname = \"bad-sev\"\nseverity = \"critical\"\npath_glob = \"**\"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a rule that fails guardrail.Compile validation")
	}
	if got := err.Error(); !strings.Contains(got, "bad-sev") {
		t.Errorf("error should name the offending rule %q, got: %v", "bad-sev", got)
	}
}

// TestLoadAcceptsRuleUsingV05OnlyFields guards the compatible-superset claim
// end to end through config.Load: a config using ONLY new v0.5 fields (no
// v0.2 fields at all) must load cleanly and route through Compile without
// error.
func TestLoadAcceptsRuleUsingV05OnlyFields(t *testing.T) {
	path := writeTOML(t, `
[[rules]]
name = "secrets"
severity = "danger"
added_pattern = "AKIA[0-9A-Z]{16}"

[[rules]]
name = "blast-radius"
min_files_changed = 25
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a config using only v0.5 fields should load cleanly, got: %v", err)
	}
	if len(cfg.Rules) != 2 || cfg.Rules[0].AddedPattern == "" || cfg.Rules[1].MinFilesChanged != 25 {
		t.Errorf("Rules = %+v, want the two v0.5-only rules decoded", cfg.Rules)
	}
}

func TestLoadMalformedTOMLErrors(t *testing.T) {
	path := writeTOML(t, "this is not [ valid toml")
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for malformed TOML")
	}
}

func TestRulesOrReturnsConfigRulesWhenPresent(t *testing.T) {
	cfg := Config{RulesSet: true, Rules: []guardrail.Rule{{Name: "only-mine"}}}
	defaults := guardrail.DefaultRules()
	got := cfg.RulesOr(defaults)
	if len(got) != 1 || got[0].Name != "only-mine" {
		t.Errorf("RulesOr = %+v, want just the config's rule", got)
	}
}

func TestRulesOrFallsBackToDefaultsWhenAbsent(t *testing.T) {
	cfg := Config{} // RulesSet false, as when [[rules]] was never in the file
	defaults := guardrail.DefaultRules()
	got := cfg.RulesOr(defaults)
	if len(got) != len(defaults) {
		t.Errorf("RulesOr = %+v, want the defaults %+v", got, defaults)
	}
}

// ---- [notifications] (P5-design.md §1.5, §2) ----

// TestLoadNotificationsAbsentTableDefaultsToEnabled: no [notifications]
// table at all must still resolve to enabled, danger-only, 10m cooldown —
// the frozen "default ON" contract.
func TestLoadNotificationsAbsentTableDefaultsToEnabled(t *testing.T) {
	path := writeTOML(t, `base = "main"`+"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Notifications.EnabledOr() {
		t.Error("EnabledOr() should be true when [notifications] is entirely absent")
	}
	if got := cfg.Notifications.SeverityOr(); got != "danger" {
		t.Errorf("SeverityOr() = %q, want danger", got)
	}
	if got := cfg.Notifications.CooldownOr(); got != 10*time.Minute {
		t.Errorf("CooldownOr() = %v, want 10m", got)
	}
}

// TestLoadNotificationsPresentTableWithoutEnabledStillDefaultsTrue: a
// [notifications] table that sets other keys but never `enabled` must still
// default to enabled — only an explicit `enabled = false` turns it off.
func TestLoadNotificationsPresentTableWithoutEnabledStillDefaultsTrue(t *testing.T) {
	path := writeTOML(t, "[notifications]\nseverity = \"warn\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Notifications.EnabledOr() {
		t.Error("EnabledOr() should default true even when [notifications] is present but doesn't set `enabled`")
	}
	if got := cfg.Notifications.SeverityOr(); got != "warn" {
		t.Errorf("SeverityOr() = %q, want warn", got)
	}
}

// TestLoadNotificationsExplicitEnabledFalseDisables is the one way to opt
// out entirely.
func TestLoadNotificationsExplicitEnabledFalseDisables(t *testing.T) {
	path := writeTOML(t, "[notifications]\nenabled = false\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notifications.EnabledOr() {
		t.Error("EnabledOr() should be false after an explicit enabled = false")
	}
}

// TestLoadNotificationsExplicitEnabledTrueIsRedundantButFine documents that
// writing `enabled = true` explicitly (redundant with the default) still
// round-trips correctly.
func TestLoadNotificationsExplicitEnabledTrueIsRedundantButFine(t *testing.T) {
	path := writeTOML(t, "[notifications]\nenabled = true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Notifications.EnabledOr() {
		t.Error("EnabledOr() should be true")
	}
}

func TestLoadNotificationsFullExampleParsesEveryField(t *testing.T) {
	path := writeTOML(t, "[notifications]\nenabled = true\nseverity = \"warn\"\ncooldown = \"5m\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Notifications.EnabledOr() {
		t.Error("EnabledOr() should be true")
	}
	if got := cfg.Notifications.SeverityOr(); got != "warn" {
		t.Errorf("SeverityOr() = %q, want warn", got)
	}
	if got := cfg.Notifications.CooldownOr(); got != 5*time.Minute {
		t.Errorf("CooldownOr() = %v, want 5m", got)
	}
}

// TestLoadNotificationsInvalidSeverityErrorsAndNamesIt pins the enum
// validation: anything other than "danger"/"warn" (blank excepted) is a
// load error naming the offending value.
func TestLoadNotificationsInvalidSeverityErrorsAndNamesIt(t *testing.T) {
	path := writeTOML(t, "[notifications]\nseverity = \"critical\"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an invalid notifications.severity")
	}
	if got := err.Error(); !strings.Contains(got, "critical") || !strings.Contains(got, "severity") {
		t.Errorf("error = %q, want it to name the offending value and field", got)
	}
}

// TestLoadNotificationsUnknownKeyErrorsAndNamesIt: a typo under
// [notifications] must fail exactly like every other hand-edited-config
// typo (meta.Undecoded() already catches this at the top level; this pins
// that a nested table is no exception).
func TestLoadNotificationsUnknownKeyErrorsAndNamesIt(t *testing.T) {
	path := writeTOML(t, "[notifications]\nseverety = \"warn\"\n") // typo of "severity"
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a mistyped [notifications] key")
	}
	if got := err.Error(); !strings.Contains(got, "severety") {
		t.Errorf("error should name the offending key %q, got: %v", "severety", got)
	}
}

// TestCooldownOrLeavesPositiveValuesUntouched is CooldownOr's control case:
// a real configured cooldown must pass through exactly.
func TestCooldownOrLeavesPositiveValuesUntouched(t *testing.T) {
	n := NotificationsConfig{Cooldown: 90 * time.Second}
	if got := n.CooldownOr(); got != 90*time.Second {
		t.Errorf("CooldownOr() = %v, want 90s unchanged", got)
	}
}

// ---- include_repos / exclude_repos (repo discovery filter) ----

// TestLoadParsesIncludeAndExcludeRepos pins the plain happy path: both keys
// decode into their own string slices, independent of each other and of
// Roots.
func TestLoadParsesIncludeAndExcludeRepos(t *testing.T) {
	path := writeTOML(t, `
roots = ["/home/nav/code"]
include_repos = ["api-*", "web-*"]
exclude_repos = ["*-archive", "?tmp"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	wantInclude := []string{"api-*", "web-*"}
	if !reflect.DeepEqual(cfg.IncludeRepos, wantInclude) {
		t.Errorf("IncludeRepos = %v, want %v", cfg.IncludeRepos, wantInclude)
	}
	wantExclude := []string{"*-archive", "?tmp"}
	if !reflect.DeepEqual(cfg.ExcludeRepos, wantExclude) {
		t.Errorf("ExcludeRepos = %v, want %v", cfg.ExcludeRepos, wantExclude)
	}
}

// TestLoadAbsentIncludeExcludeReposAreNilNotError: neither key is required —
// a config with no discovery filter at all must load cleanly with both
// slices nil (RepoPasses's "include empty -> everything passes" default).
func TestLoadAbsentIncludeExcludeReposAreNilNotError(t *testing.T) {
	path := writeTOML(t, `base = "main"`+"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IncludeRepos != nil || cfg.ExcludeRepos != nil {
		t.Errorf("IncludeRepos/ExcludeRepos = %v/%v, want both nil when absent", cfg.IncludeRepos, cfg.ExcludeRepos)
	}
}

// TestLoadBadIncludeRepoGlobErrorsAndNamesPattern: an include_repos entry
// that filepath.Match itself rejects (ErrBadPattern) must fail config load
// fail-closed, same as every other hand-edited-config typo — and the error
// must name the offending pattern so the operator can find it.
func TestLoadBadIncludeRepoGlobErrorsAndNamesPattern(t *testing.T) {
	path := writeTOML(t, `include_repos = ["["]`+"\n") // unterminated char class
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an invalid include_repos glob")
	}
	if got := err.Error(); !strings.Contains(got, "[") {
		t.Errorf("error should name the offending pattern, got: %v", got)
	}
}

// TestLoadBadExcludeRepoGlobErrorsAndNamesPattern is the exclude_repos
// sibling of the above.
func TestLoadBadExcludeRepoGlobErrorsAndNamesPattern(t *testing.T) {
	path := writeTOML(t, `exclude_repos = ["archive-*", "["]`+"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an invalid exclude_repos glob")
	}
	if got := err.Error(); !strings.Contains(got, "[") {
		t.Errorf("error should name the offending pattern, got: %v", got)
	}
}

// TestLoadRejectsUnterminatedBracketGlobWithContent is
// TestLoadBadIncludeRepoGlobErrorsAndNamesPattern's sibling for the "[ab"
// shape specifically: an unterminated character class WITH content before
// the missing "]", as opposed to a bare "[". A config author is far more
// likely to typo a glob this way (meaning "a or b", forgetting the closing
// bracket) than to write a lone "[", so this pins that filepath.Match still
// reports ErrBadPattern for it too — not, say, some other malformed-pattern
// code path — at config LOAD time, same as every other bad pattern.
func TestLoadRejectsUnterminatedBracketGlobWithContent(t *testing.T) {
	path := writeTOML(t, `include_repos = ["[ab"]`+"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an unterminated character class with content ([ab)")
	}
	if got := err.Error(); !strings.Contains(got, "[ab") {
		t.Errorf("error should name the offending pattern, got: %v", got)
	}
}

// TestLoadValidGlobFormsAllParseCleanly exercises the glob forms the
// discovery-side table test also covers (archive-*, ?tmp, [ab]x) end to end
// through Load's validation probe — none of them should trip ErrBadPattern.
func TestLoadValidGlobFormsAllParseCleanly(t *testing.T) {
	path := writeTOML(t, `exclude_repos = ["archive-*", "?tmp", "[ab]x"]`+"\n")
	if _, err := Load(path); err != nil {
		t.Errorf("valid glob forms should load cleanly, got: %v", err)
	}
}

// ---- checkGlobSyntax: equivalence proof (the lazy-Match footgun) ----
//
// filepath.Match only surfaces ErrBadPattern LAZILY: it parses a pattern
// chunk by chunk and gives up as soon as an earlier segment fails to match
// the specific name it was called with. That means probing
// Match(pattern, "x") — validateRepoGlobs' OLD approach — can pass clean for
// a pattern like "prod-*[": the mismatch on the literal "prod-" happens
// before the dangling "[" is ever parsed, yet that same pattern errors at
// scan time against a real repo named e.g. "prod-api". The tests below
// prove checkGlobSyntax doesn't share that blind spot: it must reject a
// pattern iff Match would error for SOME name it could ever be evaluated
// against — not just the one probe name a config-load-time check happens
// to try.

// globProbeBase seeds buildGlobProbeNames — the brief's own fixed probe set.
var globProbeBase = []string{"", "x", "prod-api", "a", "ab", "a]c"}

// buildGlobProbeNames returns globProbeBase plus every prefix of pattern
// itself, and of pattern with its "\" escapes stripped: a literal chunk
// always matches its own DE-ESCAPED text, so probing with a pattern's own
// prefixes is the general way to reach as deep as Match's lazy, chunk-by-
// chunk parser can structurally go into a LATER chunk — exactly how
// "prod-api" reaches into "prod-*[" 's dangling "[".
func buildGlobProbeNames(pattern string) []string {
	names := append([]string(nil), globProbeBase...)
	for i := 1; i <= len(pattern); i++ {
		names = append(names, pattern[:i])
	}
	de := deEscapeForGlobTest(pattern)
	for i := 1; i <= len(de); i++ {
		names = append(names, de[:i])
	}
	// A "[...]" class matches CONTENT, not its own bracket syntax — a plain
	// prefix of pattern's raw text can never satisfy one (e.g. a class
	// chunk "[*]" needs a name starting with the literal "*", not "["), so
	// a pattern shaped like "[*]*[" (a valid single-char class, then a
	// dangling class after a star) would otherwise look falsely "never bad"
	// to this ground truth. bracketContentProbes below plugs that hole.
	for _, p := range bracketContentProbes(pattern) {
		for i := 1; i <= len(p); i++ {
			names = append(names, p[:i])
		}
	}
	return names
}

// bracketContentProbes returns, for every "[" in pattern, a synthetic probe
// name: pattern's own text up to that "[", plus the single content
// character the class itself would accept as its first item (skipping a
// leading "^" negation marker) — letting ground truth actually satisfy a
// bracket-class CHUNK the same way a real repo name eventually could, the
// same "use the pattern's own content to reach deeper" idea
// buildGlobProbeNames already applies to plain literal chunks.
func bracketContentProbes(pattern string) []string {
	var out []string
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '[' {
			continue
		}
		j := i + 1
		if j < len(pattern) && pattern[j] == '^' {
			j++
		}
		if j >= len(pattern) {
			continue
		}
		_, n := utf8.DecodeRuneInString(pattern[j:])
		if j+n > len(pattern) {
			continue
		}
		out = append(out, pattern[:i]+pattern[j:j+n])
	}
	return out
}

// deEscapeForGlobTest strips one backslash before each following byte — a
// rough, test-only unescape, good enough to derive additional candidate
// probe names: it can only make matchErrorsForSomeName MORE complete (find
// more real Match errors), never less accurate for any name actually tried.
func deEscapeForGlobTest(pattern string) string {
	b := make([]byte, 0, len(pattern))
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '\\' && i+1 < len(pattern) {
			i++
		}
		b = append(b, pattern[i])
	}
	return string(b)
}

// matchErrorsForSomeName is the ground truth checkGlobSyntax must track:
// whether filepath.Match(pattern, name) errors for SOME name in
// buildGlobProbeNames(pattern).
func matchErrorsForSomeName(pattern string) bool {
	for _, n := range buildGlobProbeNames(pattern) {
		if _, err := filepath.Match(pattern, n); err != nil {
			return true
		}
	}
	return false
}

// TestCheckGlobSyntaxEquivalentToLazyMatchAcrossTrickyCorpus is FIX 1's
// equivalence proof, over the brief's own corpus: a[, [ab, prod-*[, a\, \,
// [], []], [^], [a-], [--0], a[b\]c, *[, ?[x (all genuinely malformed) and
// a set of validly-formed patterns (which must stay accepted).
func TestCheckGlobSyntaxEquivalentToLazyMatchAcrossTrickyCorpus(t *testing.T) {
	tricky := []string{
		"a[", "[ab", "prod-*[", `a\`, `\`, "[]", "[]]", "[^]", "[a-]", "[--0]",
		`a[b\]c`, "*[", "?[x",
	}
	valid := []string{
		"[a-z]", "prod-*", "archive-*", "?tmp", "[ab]x", "[^ab]x", `a\*b`,
		`\*`, `\?`, `\[`, "a**b", "[!ab]", "", "*", "?",
		// FIX 7/8: an escaped MULTIBYTE rune inside a class, its negated
		// form, and a range between two multibyte runes — all genuinely
		// valid per filepath.Match, but over-rejected by the bug FIX 7
		// fixed (skipGlobClassItem used to treat an escaped char as exactly
		// one BYTE rather than decoding the full rune, so "€"'s trailing
		// continuation bytes read back as their own invalid item).
		`[\€]`, `[^\€]`, `[€-☃]`,
	}

	for _, pat := range tricky {
		if !matchErrorsForSomeName(pat) {
			t.Fatalf("test bug: ground truth says tricky pattern %q is never bad — corpus needs a probe name that reaches it", pat)
		}
		if err := checkGlobSyntax(pat); err == nil {
			t.Errorf("checkGlobSyntax(%q) = nil, want an error (Match errors for some reachable name)", pat)
		}
	}
	for _, pat := range valid {
		if err := checkGlobSyntax(pat); err != nil {
			t.Errorf("checkGlobSyntax(%q) = %v, want nil (a genuinely valid glob)", pat, err)
		}
		if matchErrorsForSomeName(pat) {
			t.Fatalf("test bug: ground truth says supposedly-valid pattern %q IS bad", pat)
		}
	}
}

// globSyntaxAlphabet is the generation alphabet for the exhaustive
// equivalence test below. FIX 8: the original alphabet here was ASCII-only,
// which is exactly why that test stayed green straight through FIX 7's
// escaped-multibyte-rune bug — it never generated a pattern shaped like
// "[\€]". "€" (3 bytes) and "é" (2 bytes) exercise multibyte runes of two
// different widths, both standalone and combined with the ASCII glob
// metacharacters.
var globSyntaxAlphabet = []string{"a", "-", "[", "]", "^", `\`, "*", "?", "€", "é"}

// genGlobPatterns returns every string of length 1..maxLen built from
// alphabet (a rune/multi-rune-string alphabet, not a byte one — string
// concatenation keeps each multibyte entry intact).
func genGlobPatterns(alphabet []string, maxLen int) []string {
	var out []string
	var rec func(cur string, depth int)
	rec = func(cur string, depth int) {
		if depth > 0 {
			out = append(out, cur)
		}
		if depth == maxLen {
			return
		}
		for _, c := range alphabet {
			rec(cur+c, depth+1)
		}
	}
	rec("", 0)
	return out
}

// TestCheckGlobSyntaxEquivalentToLazyMatchExhaustiveWithMultibyte is FIX 8:
// generates every pattern up to length 4 over globSyntaxAlphabet (which
// includes multibyte runes) and checks checkGlobSyntax against the same
// Match-based ground truth as the corpus test above — both directions, no
// false-accept and no false-reject — closing the false-green gap an
// ASCII-only corpus left open for FIX 7's bug.
func TestCheckGlobSyntaxEquivalentToLazyMatchExhaustiveWithMultibyte(t *testing.T) {
	patterns := genGlobPatterns(globSyntaxAlphabet, 5)
	t.Logf("checking %d generated patterns (incl. multibyte runes)", len(patterns))

	mismatches := 0
	for _, pat := range patterns {
		want := matchErrorsForSomeName(pat)
		got := checkGlobSyntax(pat) != nil
		if want != got {
			mismatches++
			if mismatches <= 20 {
				t.Errorf("pattern %q: checkGlobSyntax.bad=%v, groundTruth(Match).bad=%v", pat, got, want)
			}
		}
	}
	if mismatches > 0 {
		t.Fatalf("%d/%d generated patterns disagreed with ground truth (showing up to 20)", mismatches, len(patterns))
	}
}

// TestCheckGlobSyntaxAgreesWithMatchOnARawInvalidUTF8Byte is FIX 8's
// direct-function-level check: TOML can never actually deliver a raw
// invalid UTF-8 byte in a config string (BurntSushi's decoder requires
// valid UTF-8), so this bypasses Load/writeTOML entirely and calls
// checkGlobSyntax directly — proving it still agrees with filepath.Match's
// own verdict on such a byte (both bare and backslash-escaped, inside a
// class), not just on patterns a config author could actually type.
func TestCheckGlobSyntaxAgreesWithMatchOnARawInvalidUTF8Byte(t *testing.T) {
	patterns := []string{"[\xff]", "[\\" + "\xff" + "]"}
	for _, pat := range patterns {
		if !matchErrorsForSomeName(pat) {
			t.Fatalf("test bug: ground truth says %q is never bad", pat)
		}
		if err := checkGlobSyntax(pat); err == nil {
			t.Errorf("checkGlobSyntax(%q) = nil, want an error (matches filepath.Match's own verdict on an invalid UTF-8 byte)", pat)
		}
	}
}

// TestCheckGlobSyntaxCatchesPatternTheOldProbeMissed is FIX 1's headline
// regression pin: "prod-*[" passes the OLD filepath.Match(pat, "x") probe
// clean (the mismatch on "prod-" happens before the dangling "[" is ever
// parsed) yet errors against a real repo name like "prod-api" — this locks
// in both halves of that premise, then asserts checkGlobSyntax rejects it
// outright and Load() refuses to start over it end to end.
func TestCheckGlobSyntaxCatchesPatternTheOldProbeMissed(t *testing.T) {
	const pat = "prod-*["
	if _, err := filepath.Match(pat, "x"); err != nil {
		t.Fatalf("test premise broken: Match(%q, \"x\") now errors (%v) — pick a different probe-blind pattern", pat, err)
	}
	if _, err := filepath.Match(pat, "prod-api"); err == nil {
		t.Fatalf("test premise broken: Match(%q, \"prod-api\") no longer errors — pick a different probe-blind pattern", pat)
	}
	if err := checkGlobSyntax(pat); err == nil {
		t.Error(`checkGlobSyntax("prod-*[") = nil, want an error`)
	}

	path := writeTOML(t, `exclude_repos = ["prod-*["]`+"\n")
	if _, err := Load(path); err == nil {
		t.Error("Load should refuse to start over an exclude_repos pattern that only errors for SOME repo names, not just a single config-load probe")
	}
}
