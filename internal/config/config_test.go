package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
