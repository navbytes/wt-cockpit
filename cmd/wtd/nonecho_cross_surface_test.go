package main

// Cross-surface non-echo invariant (P5-design.md §1.1's "THE NON-ECHO
// INVARIANT", exercised end to end rather than per-package): a planted
// secret's raw bytes must appear in ZERO of a guardrail hit's Message, the
// guardrail.tripped event payload (JSON-marshaled exactly as it would ride
// the SSE stream), the notifier's exec argv, and the /api/rules JSON
// response. testGit/testGitEnv/mustResolver come from main_test.go;
// writeStubNotifier/readStubInvocations from notify_smoke_test.go (same
// package, per this repo's "test helpers aren't importable across packages"
// convention).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/engine"
	"github.com/navbytes/wt-cockpit/internal/gitbackend"
	"github.com/navbytes/wt-cockpit/internal/guardrail"
	"github.com/navbytes/wt-cockpit/internal/model"
	"github.com/navbytes/wt-cockpit/internal/notify"
	"github.com/navbytes/wt-cockpit/internal/registry"
	"github.com/navbytes/wt-cockpit/internal/store"
)

// drainNonEchoEvents collects every event received on sub within wait —
// mirrors internal/engine/engine_test.go's own drainEvents (not importable
// across packages).
func drainNonEchoEvents(sub <-chan model.Event, wait time.Duration) []model.Event {
	var out []model.Event
	timeout := time.After(wait)
	for {
		select {
		case e := <-sub:
			out = append(out, e)
		case <-timeout:
			return out
		}
	}
}

// TestSecretNeverAppearsAcrossHitMessageEventPayloadNotifierArgvAndRulesJSON
// is the PROBE's cross-surface non-echo check, driven over a real engine +
// real registry + real notifier (stub binary on PATH) + real HTTP handlers —
// no single package's unit test can see all four surfaces at once.
func TestSecretNeverAppearsAcrossHitMessageEventPayloadNotifierArgvAndRulesJSON(t *testing.T) {
	const secret = "AKIA" + "IOSFODNN7EXAMPLE"

	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "add", ".")
	testGit(t, repo, "commit", "-q", "-m", "init")

	wt := filepath.Join(root, "repo-feature")
	testGit(t, repo, "worktree", "add", "-q", "-b", "feature", wt)

	reg := registry.New()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	be := gitbackend.NewCLIWithEnv(testGitEnv())
	gr := mustResolver(t, guardrail.DefaultRules())
	eng := engine.New(engine.Config{Roots: []string{root}, MaxDepth: 4, ActivityWindow: 30 * time.Second}, be, reg, st, gr)

	// Subscribe before the cold-start scan completes so the hit planted AFTER
	// it publishes as a genuinely new guardrail.tripped event.
	sub, cancel := reg.Subscribe(16)
	defer cancel()
	if err := eng.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	drainNonEchoEvents(sub, 200*time.Millisecond) // cold-start gate: nothing published yet

	// Plant the secret in a brand new file — trips secrets-pattern (danger).
	if err := os.WriteFile(filepath.Join(wt, "config.go"), []byte("package app\n\nconst awsKey = \""+secret+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := eng.RefreshOne(context.Background(), wt); err != nil {
		t.Fatal(err)
	}

	var featID string
	for _, w := range eng.List() {
		if w.Branch == "feature" {
			featID = w.ID
		}
	}
	if featID == "" {
		t.Fatalf("feature worktree not found: %+v", eng.List())
	}

	// ---- surface 1: hit.Message ----
	var hitMsg string
	var sawSecretsPattern bool
	for _, w := range eng.List() {
		if w.ID != featID {
			continue
		}
		for _, h := range w.Guardrails {
			if h.Rule == "secrets-pattern" {
				sawSecretsPattern = true
				hitMsg = h.Message
			}
		}
	}
	if !sawSecretsPattern {
		t.Fatal("precondition: the planted secret should trip secrets-pattern")
	}
	if strings.Contains(hitMsg, secret) {
		t.Errorf("hit.Message contains the planted secret: %q", hitMsg)
	}

	// ---- surface 2: the guardrail.tripped event, JSON-marshaled exactly as
	// it would ride the SSE stream ----
	events := drainNonEchoEvents(sub, time.Second)
	var eventJSON []byte
	for _, e := range events {
		if e.Type == model.EventGuardrail && e.Hit != nil && e.Hit.Rule == "secrets-pattern" {
			b, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			eventJSON = b
		}
	}
	if eventJSON == nil {
		t.Fatalf("expected a guardrail.tripped event for the newly-planted secret, got %+v", events)
	}
	if strings.Contains(string(eventJSON), secret) {
		t.Errorf("guardrail.tripped event JSON contains the planted secret: %s", eventJSON)
	}

	// ---- surface 3: /api/worktrees and /api/rules, via the real HTTP
	// handlers (not a hand-rolled marshal) ----
	srv := &server{eng: eng}
	handler := srv.routes()

	wtRec := httptest.NewRecorder()
	handler.ServeHTTP(wtRec, httptest.NewRequest(http.MethodGet, "/api/worktrees", nil))
	if strings.Contains(wtRec.Body.String(), secret) {
		t.Error("/api/worktrees response body contains the planted secret")
	}

	rulesRec := httptest.NewRecorder()
	handler.ServeHTTP(rulesRec, httptest.NewRequest(http.MethodGet, "/api/rules?id="+featID, nil))
	if strings.Contains(rulesRec.Body.String(), secret) {
		t.Error("/api/rules response body contains the planted secret (it structurally never should: guardrail.Effective only ever carries rule DEFINITIONS, never hit data — this pins that guarantee empirically)")
	}

	// ---- surface 4: the notifier's exec argv, via a real stub binary on
	// PATH (prepended, not replacing PATH, so the stub script's own use of
	// external commands still resolves) ----
	stubDir := t.TempDir()
	logPath := filepath.Join(stubDir, "argv.log")
	writeStubNotifier(t, stubDir, logPath)
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	notifyCh, notifyCancel := reg.Subscribe(16)
	defer notifyCancel()
	n := notify.New(notify.Config{Enabled: true, Severity: "danger"}, notifyLookup(reg))
	notifyCtx, notifyStop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(notifyCtx, notifyCh); close(done) }()

	// A second planted secret in a new file: a fresh (rule, file) key the
	// notifier's own subscription (started just now) will see live.
	if err := os.WriteFile(filepath.Join(wt, "config2.go"), []byte("package app\n\nconst awsKey2 = \""+secret+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := eng.RefreshOne(context.Background(), wt); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second) // real 5s coalescing window + slack
	for time.Now().Before(deadline) {
		if len(readStubInvocations(t, logPath)) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	notifyStop()
	<-done

	invocations := readStubInvocations(t, logPath)
	if len(invocations) == 0 {
		t.Fatal("expected at least one notifier invocation for the second planted secret")
	}
	for _, argv := range invocations {
		joined := strings.Join(argv, "\x1f")
		if strings.Contains(joined, secret) {
			t.Errorf("notifier argv contains the planted secret: %v", argv)
		}
	}
}
