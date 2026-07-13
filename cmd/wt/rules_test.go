package main

// wt rules: the human table's provenance columns and the --json payload,
// end-to-end against a fake daemon on a real Unix socket (requireWtBin/
// unixSocketServer/shortSocketDir come from integration_test.go, same
// package).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/guardrail"
	"github.com/navbytes/wt-cockpit/internal/model"
)

// TestWtRulesRoundTrip runs the real wt binary twice (human table, then
// --json) against a fake wtd serving a pack-augmented rule set, proving the
// full CLI surface — not just renderRules/conditionsSummary in isolation —
// shows provenance (default vs pack) and the pack's own path/status.
func TestWtRulesRoundTrip(t *testing.T) {
	bin := requireWtBin(t)
	sockPath := filepath.Join(shortSocketDir(t), "wtd.sock")
	const wtID = "api-server-feature"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": model.ProtocolVersion, "version": "x", "goVersion": "go1.24"})
	})
	mux.HandleFunc("/api/rules", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") != wtID {
			http.Error(w, "unknown worktree id", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{
			"worktreeId": "api-server-feature",
			"repoPath": "/repo/api-server",
			"packPath": "/repo/api-server/.wtcockpit.toml",
			"packStatus": "ok",
			"rules": [
				{"name":"touches-payments","severity":"danger","pathGlobs":["internal/payments/**"],"message":"touches payment code","source":"pack"},
				{"name":"large-deletion","severity":"warn","minNetDeleted":80,"message":"large net deletion in one file","source":"default"}
			]
		}`)
	})
	ts := unixSocketServer(t, sockPath, mux)
	defer ts.Close()

	run := func(args ...string) (string, string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "WTD_SOCKET="+sockPath)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	out, errOut, err := run("rules", wtID)
	if err != nil {
		t.Fatalf("wt rules failed: %v\nstdout=%s\nstderr=%s", err, out, errOut)
	}
	for _, want := range []string{
		"touches-payments", "danger", "pack", "internal/payments/**",
		"large-deletion", "warn", "default", "min_net_deleted=80",
		"/repo/api-server/.wtcockpit.toml", "ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("wt rules output missing %q:\n%s", want, out)
		}
	}

	jsonOut, errOut, err := run("rules", wtID, "--json")
	if err != nil {
		t.Fatalf("wt rules --json failed: %v\nstderr=%s", err, errOut)
	}
	var eff guardrail.Effective
	if err := json.Unmarshal([]byte(jsonOut), &eff); err != nil {
		t.Fatalf("wt rules --json did not print valid JSON: %v\noutput:\n%s", err, jsonOut)
	}
	if eff.WorktreeID != wtID || eff.PackStatus != "ok" || eff.PackPath == "" {
		t.Errorf("wt rules --json decoded = %+v", eff)
	}
	if len(eff.Rules) != 2 || eff.Rules[0].Source != "pack" || eff.Rules[1].Source != "default" {
		t.Errorf("wt rules --json Rules = %+v, want [pack, default]", eff.Rules)
	}
}

// TestWtRulesUnknownIDPrintsDaemonErrorAndExitsNonZero mirrors the CLI's
// existing not-found error voice (e.g. wt diff/wt comments on a bad id): the
// daemon's exact message on stderr, non-zero exit.
func TestWtRulesUnknownIDPrintsDaemonErrorAndExitsNonZero(t *testing.T) {
	bin := requireWtBin(t)
	sockPath := filepath.Join(shortSocketDir(t), "wtd.sock")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": model.ProtocolVersion, "version": "x", "goVersion": "go1.24"})
	})
	mux.HandleFunc("/api/rules", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown worktree id", http.StatusNotFound)
	})
	ts := unixSocketServer(t, sockPath, mux)
	defer ts.Close()

	cmd := exec.Command(bin, "rules", "no-such-id")
	cmd.Env = append(os.Environ(), "WTD_SOCKET="+sockPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected wt rules to exit non-zero for an unknown id")
	}
	if !strings.Contains(stderr.String(), "unknown worktree id") {
		t.Errorf("stderr = %q, want the daemon's error message", stderr.String())
	}
}

// ---- conditionsSummary / renderRules (in-process, no socket needed) ----

func TestConditionsSummaryRendersEverySetField(t *testing.T) {
	r := guardrail.Rule{
		PathGlobs:       []string{"a/**", "b/**"},
		MinChangedLines: 100,
		Status:          "deleted",
	}
	got := conditionsSummary(r)
	for _, want := range []string{"path_globs=a/**,b/**", "min_changed_lines=100", "status=deleted"} {
		if !strings.Contains(got, want) {
			t.Errorf("conditionsSummary(%+v) = %q, want it to contain %q", r, got, want)
		}
	}
}

func TestConditionsSummaryEmptyRuleReadsNone(t *testing.T) {
	if got := conditionsSummary(guardrail.Rule{}); got != "(none)" {
		t.Errorf("conditionsSummary(zero Rule) = %q, want \"(none)\"", got)
	}
}

func TestEffectiveSeverityDefaultsBlankToWarn(t *testing.T) {
	if got := effectiveSeverity(""); got != "warn" {
		t.Errorf("effectiveSeverity(\"\") = %q, want warn", got)
	}
	if got := effectiveSeverity("danger"); got != "danger" {
		t.Errorf("effectiveSeverity(danger) = %q, want danger unchanged", got)
	}
}
