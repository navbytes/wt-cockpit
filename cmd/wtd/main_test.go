package main

import (
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/config"
)

// These test the merge logic directly (per the design note: testing flag.Parse
// itself is awkward, since flag.CommandLine is a package-level global).

func TestMergeSettingExplicitFlagWinsOverConfig(t *testing.T) {
	got := mergeSetting("Y", true, "X")
	if got != "Y" {
		t.Errorf("got %q, want the explicit flag value Y", got)
	}
}

func TestMergeSettingConfigWinsOverBuiltinDefaultWhenNotExplicit(t *testing.T) {
	got := mergeSetting("builtin-default", false, "from-config")
	if got != "from-config" {
		t.Errorf("got %q, want from-config", got)
	}
}

func TestMergeSettingFallsBackToBuiltinDefaultWhenConfigUnset(t *testing.T) {
	got := mergeSetting("builtin-default", false, "")
	if got != "builtin-default" {
		t.Errorf("got %q, want builtin-default", got)
	}
}

func TestMergeSettingWorksForDuration(t *testing.T) {
	if got := mergeSetting(2*time.Second, false, 5*time.Second); got != 5*time.Second {
		t.Errorf("got %v, want config's 5s", got)
	}
	if got := mergeSetting(2*time.Second, true, 5*time.Second); got != 2*time.Second {
		t.Errorf("got %v, want the explicit flag's 2s", got)
	}
}

func TestMergeRootsExplicitFlagReplacesConfigEntirely(t *testing.T) {
	got := mergeRoots([]string{"/cli/root"}, true, []string{"/cfg/a", "/cfg/b"})
	if len(got) != 1 || got[0] != "/cli/root" {
		t.Errorf("got %v, want just [/cli/root] (no merging with config roots)", got)
	}
}

func TestMergeRootsUsesConfigWhenFlagNotExplicit(t *testing.T) {
	got := mergeRoots(nil, false, []string{"/cfg/a", "/cfg/b"})
	if len(got) != 2 || got[0] != "/cfg/a" || got[1] != "/cfg/b" {
		t.Errorf("got %v, want the config roots", got)
	}
}

func TestMergeRootsFallsBackToFlagDefaultWhenNeitherSet(t *testing.T) {
	got := mergeRoots([]string{"/cwd"}, false, nil)
	if len(got) != 1 || got[0] != "/cwd" {
		t.Errorf("got %v, want the flag's own default", got)
	}
}

func TestBaseForBuildsMapFromConfigRepos(t *testing.T) {
	cfg := config.Config{Repos: map[string]config.RepoConfig{
		"/repos/api":    {Base: "develop"},
		"/repos/ignore": {}, // no override set -> must be excluded
	}}
	got := baseFor(cfg)
	if got["/repos/api"] != "develop" {
		t.Errorf(`baseFor["/repos/api"] = %q, want "develop"`, got["/repos/api"])
	}
	if _, ok := got["/repos/ignore"]; ok {
		t.Errorf("repo with no base override should be excluded, got %+v", got)
	}
}
