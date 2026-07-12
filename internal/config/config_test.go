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
state = "/home/nav/.wtcockpit/state.json"
interval = "5s"
watch = "fsnotify"

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
	if cfg.State != "/home/nav/.wtcockpit/state.json" {
		t.Errorf("State = %q", cfg.State)
	}
	if cfg.Interval != 5*time.Second {
		t.Errorf("Interval = %v, want 5s", cfg.Interval)
	}
	if cfg.Watch != "fsnotify" {
		t.Errorf("Watch = %q", cfg.Watch)
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
	want0 := guardrail.Rule{Name: "touches-migrations", Severity: "danger", PathGlob: "**/migrations/**", Message: "touches database migrations"}
	if cfg.Rules[0] != want0 {
		t.Errorf("Rules[0] = %+v, want %+v", cfg.Rules[0], want0)
	}
	want1 := guardrail.Rule{Name: "large-deletion", Severity: "warn", MinNetDeleted: 80}
	if cfg.Rules[1] != want1 {
		t.Errorf("Rules[1] = %+v, want %+v", cfg.Rules[1], want1)
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
