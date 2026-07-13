package notify

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// ---- probe / Status ----

func TestProbeDisabledNeverTouchesPATH(t *testing.T) {
	bin, path := probe(false, "darwin")
	if bin != binDisabled || path != "" {
		t.Errorf("probe(false, darwin) = (%q, %q), want (disabled, \"\")", bin, path)
	}
}

func TestProbeUnsupportedGOOSIsUnavailable(t *testing.T) {
	bin, _ := probe(true, "windows")
	if bin != binUnavailable {
		t.Errorf("probe(true, windows) = %q, want unavailable", bin)
	}
}

// TestProbeMissingBinaryOnPATHIsUnavailable forces a LookPath miss
// regardless of the host OS by pointing PATH at an empty directory, then
// probes for both platforms' binaries explicitly (goos is a parameter, so
// this is deterministic on any CI runner).
func TestProbeMissingBinaryOnPATHIsUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, goos := range []string{"darwin", "linux"} {
		bin, path := probe(true, goos)
		if bin != binUnavailable || path != "" {
			t.Errorf("probe(true, %s) with empty PATH = (%q, %q), want (unavailable, \"\")", goos, bin, path)
		}
	}
}

// TestProbeFindsStubBinaryOnPATH proves probe resolves a stub script placed
// on PATH under the platform's expected name — the exact seam the brief
// calls out ("LookPath is chosen precisely so ACs can drive a stub").
func TestProbeFindsStubBinaryOnPATH(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want binKind
	}{
		{"darwin", binOsascript},
		{"linux", binNotifySend},
	} {
		dir := t.TempDir()
		writeStub(t, dir, wantBinaryName(tc.goos), "#!/bin/sh\nexit 0\n")
		t.Setenv("PATH", dir)

		bin, path := probe(true, tc.goos)
		if bin != tc.want {
			t.Errorf("probe(true, %s) bin = %q, want %q", tc.goos, bin, tc.want)
		}
		if path == "" {
			t.Errorf("probe(true, %s) path = empty, want the stub's resolved path", tc.goos)
		}
	}
}

func TestStatusReflectsProbeResult(t *testing.T) {
	n := newNotifier(Config{Enabled: false}, noopLookup, time.Now, time.After)
	if got := n.Status(); got != "disabled (config)" {
		t.Errorf("Status() = %q, want %q", got, "disabled (config)")
	}
}

// ---- severity filter ----

func noopLookup(id string) (string, string, bool) { return "", "", false }

func TestPassesSeverityDefaultFloorIsDangerOnly(t *testing.T) {
	n := newNotifier(Config{Enabled: false, Severity: "danger"}, noopLookup, time.Now, time.After)
	if !n.passesSeverity("danger") {
		t.Error("danger must pass the default (danger) floor")
	}
	if n.passesSeverity("warn") {
		t.Error("warn must NOT pass the default (danger) floor")
	}
}

func TestPassesSeverityWarnFloorAdmitsBoth(t *testing.T) {
	n := newNotifier(Config{Enabled: false, Severity: "warn"}, noopLookup, time.Now, time.After)
	if !n.passesSeverity("danger") {
		t.Error("danger must pass the warn floor")
	}
	if !n.passesSeverity("warn") {
		t.Error("warn must pass the warn floor")
	}
}

// TestIngestIgnoresNonGuardrailEvents pins that Run's ingest step never
// mistakes an unrelated bus event (e.g. worktree.upserted) for a guardrail
// hit.
func TestIngestIgnoresNonGuardrailEvents(t *testing.T) {
	n := newNotifier(Config{Enabled: false, Severity: "warn"}, noopLookup, time.Now, time.After)
	if armed := n.ingest(model.Event{Type: model.EventWorktreeUpserted}); armed {
		t.Error("a non-guardrail event must never arm the coalescing window")
	}
	if armed := n.ingest(model.Event{Type: model.EventGuardrail, Hit: nil}); armed {
		t.Error("a guardrail event with a nil Hit must never arm the coalescing window")
	}
}

// TestRunWithNoBinaryLogsExactlyOneInfoLineWithExpectedContent closes a gap
// TestRunWithNoBinaryLogsOnceAndReturnsWithoutBlocking leaves open: that test
// proves Run doesn't block, but never actually inspects what (if anything)
// got logged. This asserts the log content the "missing binary -> one info
// log" contract (P5-design.md §1.5's "Graceful absence") promises.
func TestRunWithNoBinaryLogsExactlyOneInfoLineWithExpectedContent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	n := New(Config{Enabled: true, Severity: "danger"}, noopLookup)

	ch := make(chan model.Event, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	logs := captureLogs(t, func() { n.Run(ctx, ch) })
	if got := strings.Count(logs, "desktop notifications unavailable"); got != 1 {
		t.Errorf("missing-binary Run should log exactly one line about unavailable notifications, got %d; logs=%s", got, logs)
	}
	if !strings.Contains(logs, "notifier binary not found in PATH") {
		t.Errorf("expected the log to mention the binary wasn't found in PATH, got: %s", logs)
	}
}

// ---- Run: missing binary / disabled ----

// TestRunWithNoBinaryLogsOnceAndReturnsWithoutBlocking pins the "missing
// binary -> disabled once, no crash" contract: Run must return promptly
// (never block reading ch forever) when no notifier binary is available.
func TestRunWithNoBinaryLogsOnceAndReturnsWithoutBlocking(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // guarantees a LookPath miss regardless of host OS
	n := New(Config{Enabled: true, Severity: "danger"}, noopLookup)
	if n.active() {
		t.Fatalf("notifier should be inactive with no binary on PATH, got bin=%q", n.bin)
	}

	ch := make(chan model.Event, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		n.Run(ctx, ch)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return promptly for an inactive notifier")
	}
	// The bus must keep flowing: sending into ch (as the registry would to any
	// other subscriber) must never block just because this one gave up reading.
	select {
	case ch <- model.Event{Type: model.EventGuardrail}:
	default:
		t.Error("ch should still accept sends; Run returning must not wedge the channel")
	}
}

// TestRunDisabledByConfigDoesNotBlock: enabled=false must return promptly
// too, without ever touching PATH.
func TestRunDisabledByConfigDoesNotBlock(t *testing.T) {
	n := New(Config{Enabled: false}, noopLookup)
	if n.Status() != "disabled (config)" {
		t.Fatalf("Status() = %q, want disabled (config)", n.Status())
	}
	ch := make(chan model.Event)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		n.Run(ctx, ch)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run(disabled) did not return promptly")
	}
}

// ---- Run end to end against a stub notifier binary ----

// writeStub writes an executable shell script named name into dir. body is
// the script's contents (a "#!/bin/sh" shebang line and beyond).
func writeStub(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// argvLogEnd marks the end of one invocation's argv in the stub's log file
// (see stubArgvLogger).
const argvLogEnd = "===END===\n"

// stubArgvLogger writes an executable stub — named per GOOS's expected
// notifier binary — that appends its argv to logPath: one element per line,
// followed by the argvLogEnd marker. This line-per-element format (rather
// than e.g. JSON, which would need real shell-side escaping) is safe here
// specifically because every argv element Run ever execs has already passed
// through Sanitize, which strips raw newlines — so no element can ever
// contain the line delimiter itself. No shell reinterprets "$a"'s content as
// anything other than data (printf '%s' with a quoted variable is literal),
// so this harness is itself injection-safe.
func stubArgvLogger(t *testing.T, dir, logPath string) {
	t.Helper()
	name := wantBinaryName(runtime.GOOS)
	if name == "" {
		t.Skipf("no notifier binary defined for GOOS %q", runtime.GOOS)
	}
	script := `#!/bin/sh
{
for a in "$@"; do
  printf '%s\n' "$a"
done
printf '===END===\n'
} >> "` + logPath + `"
exit 0
`
	writeStub(t, dir, name, script)
}

// readArgvLog decodes logPath's line-per-element, argvLogEnd-delimited
// records (see stubArgvLogger) into one []string per invocation.
func readArgvLog(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	content := string(data)
	var out [][]string
	for {
		i := strings.Index(content, argvLogEnd)
		if i < 0 {
			break
		}
		chunk := content[:i]
		content = content[i+len(argvLogEnd):]
		var argv []string
		if chunk != "" {
			argv = strings.Split(chunk, "\n")
			// The loop's final printf leaves a trailing "\n" right before the
			// marker, which Split turns into one trailing "" — an artifact of
			// the delimiter, not a real (empty-string) argv element.
			if len(argv) > 0 && argv[len(argv)-1] == "" {
				argv = argv[:len(argv)-1]
			}
		}
		out = append(out, argv)
	}
	return out
}

// fakeAfter is Run's window-timer indirection for tests: calling after()
// records the channel it handed back and signals armed so a test can block
// until Run has actually armed the coalescing window (instead of sleep-
// polling), then trigger() fires it on demand. Only one outstanding timer is
// supported — exactly Run's own usage (a single windowC variable).
type fakeAfter struct {
	mu    sync.Mutex
	ch    chan time.Time
	armed chan struct{}
}

func newFakeAfter() *fakeAfter {
	return &fakeAfter{armed: make(chan struct{}, 64)}
}

func (f *fakeAfter) after(time.Duration) <-chan time.Time {
	f.mu.Lock()
	f.ch = make(chan time.Time, 1)
	ch := f.ch
	f.mu.Unlock()
	f.armed <- struct{}{}
	return ch
}

// waitArmed blocks until after() has been called at least once since the
// last waitArmed/drain, or fails the test on timeout. 10s (not e.g. 2s) is
// deliberate slack for a heavily loaded machine (the full `go test ./...`
// run spawns many concurrent, git-heavy package test binaries) — this is a
// pure in-process goroutine signal with no subprocess involved, so it
// returns almost instantly in the common case regardless.
func (f *fakeAfter) waitArmed(t *testing.T) {
	t.Helper()
	select {
	case <-f.armed:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the coalescing window to arm")
	}
}

// assertNeverArms fails the test if after() is called within d — used to
// prove a filtered-out or cooldown-suppressed event never starts a window.
func (f *fakeAfter) assertNeverArms(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-f.armed:
		t.Fatal("expected the coalescing window to NOT arm, but it did")
	case <-time.After(d):
	}
}

func (f *fakeAfter) trigger() {
	f.mu.Lock()
	ch := f.ch
	f.mu.Unlock()
	ch <- time.Now()
}

// runNotifierForTest starts n.Run in its own goroutine over ch and returns a
// function that cancels it and blocks until the goroutine has actually
// exited — the clean-shutdown, no-leaked-goroutine contract the race
// detector is asked to certify.
func runNotifierForTest(n *Notifier, ch <-chan model.Event) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		n.Run(ctx, ch)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

// waitForArgvCount polls logPath (there is no channel-based signal for "an
// external process finished writing its file") until it has at least want
// invocations, bounded, failing the test on timeout. The bound (12s) is
// deliberately well past execTimeout (5s, fire's own exec.CommandContext
// cap): too tight a bound here would fire t.Fatal while a legitimately-
// still-running (not stuck) exec is merely slow under a loaded machine,
// whose deferred stop() would then cancel the shared ctx and SIGKILL it —
// turning a test-harness timing issue into a misleading "signal: killed"
// log instead of a clean, patient pass.
func waitForArgvCount(t *testing.T, logPath string, want int) [][]string {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		got := readArgvLog(t, logPath)
		if len(got) >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d stub invocation(s), got %d", want, len(readArgvLog(t, logPath)))
	return nil
}

func joinArgv(argv []string) string { return strings.Join(argv, "\x1f") }

// waitForChannelDrained blocks until ch has no buffered events left (Run has
// dequeued them all), bounded, failing the test on timeout.
func waitForChannelDrained(t *testing.T, ch chan model.Event) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(ch) == 0 {
			return
		}
		time.Sleep(1 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the event channel to drain, %d events still buffered", len(ch))
}

// TestRunFiresStubOnDangerHitWithExpectedArgv is the brief's headline
// end-to-end AC: a real danger guardrail.tripped event, driven through a
// real bus-shaped channel and Run's real coalescing, produces exactly one
// stub invocation whose argv contains the repo/name and the hit's message,
// no secret/shell-injection shape, and the fixed argv for this platform.
func TestRunFiresStubOnDangerHitWithExpectedArgv(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	stubArgvLogger(t, dir, logPath)
	t.Setenv("PATH", dir)

	lookup := func(id string) (string, string, bool) {
		if id == "wt1" {
			return "api-server", "auth-refactor", true
		}
		return "", "", false
	}

	n := New(Config{Enabled: true, Severity: "danger", Cooldown: 10 * time.Minute}, lookup)
	if !n.active() {
		t.Fatalf("notifier should be active with the stub on PATH, got bin=%q", n.bin)
	}
	fa := newFakeAfter()
	n.after = fa.after

	ch := make(chan model.Event, 8)
	stop := runNotifierForTest(n, ch)
	defer stop()

	ch <- model.Event{
		Type: model.EventGuardrail,
		ID:   "wt1",
		Hit:  &model.GuardrailHit{Rule: "secrets-pattern", File: "config.go", Severity: "danger", Message: "secrets-shaped string (known token pattern)"},
	}
	fa.waitArmed(t)
	fa.trigger()

	argvs := waitForArgvCount(t, logPath, 1)
	if len(argvs) != 1 {
		t.Fatalf("invocations = %d, want exactly 1", len(argvs))
	}
	joined := joinArgv(argvs[0])
	for _, want := range []string{"api-server/auth-refactor", "secrets-shaped string (known token pattern)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q missing %q", argvs[0], want)
		}
	}
	for _, bad := range []string{"AKIA", ";", "$(", "`", "\n"} {
		if strings.Contains(joined, bad) {
			t.Errorf("argv %q must not contain %q (secret material or shell metachar)", argvs[0], bad)
		}
	}

	// Recorded argv is exactly what the exec'd process received as $1.. (its
	// own argv[0] — the program name/path — is consumed by exec.Command and
	// never reaches the stub's "$@"), so it must equal buildArgv*'s output
	// minus that leading program-name element.
	wantTitle, wantBody := "wt-cockpit — api-server/auth-refactor", "secrets-shaped string (known token pattern)"
	switch n.bin {
	case binOsascript:
		assertArgvEqual(t, argvs[0], buildArgvDarwin(wantTitle, wantBody)[1:])
	case binNotifySend:
		assertArgvEqual(t, argvs[0], buildArgvLinux(wantTitle, wantBody, "critical")[1:])
	}
}

// TestRunCollapsesBurstAcrossManyWorktreesIntoOneStormSummary drives hits
// for 5 distinct worktrees within one window and asserts exactly one stub
// invocation (the storm summary), never 5.
func TestRunCollapsesBurstAcrossManyWorktreesIntoOneStormSummary(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	stubArgvLogger(t, dir, logPath)
	t.Setenv("PATH", dir)

	n := New(Config{Enabled: true, Severity: "danger"}, noopLookup)
	fa := newFakeAfter()
	n.after = fa.after

	ch := make(chan model.Event, 16)
	stop := runNotifierForTest(n, ch)
	defer stop()

	for i, wt := range []string{"wt1", "wt2", "wt3", "wt4", "wt5"} {
		ch <- model.Event{
			Type: model.EventGuardrail,
			ID:   wt,
			Hit:  &model.GuardrailHit{Rule: "r", File: "f", Severity: "danger", Message: "hit"},
		}
		if i == 0 {
			fa.waitArmed(t) // only the first event of the window arms the timer
		}
	}
	// Wait for Run to actually drain ch before triggering the window: all 5
	// sends above are buffered (non-blocking) and racing Run's own goroutine,
	// so without this, trigger() could fire while wt2..wt5 are still sitting
	// unread in the channel — the windowC case and the ch case would then
	// both be ready in Run's select, which could pick windowC first and flush
	// with only wt1 pending. Once ch is observably drained, Run cannot reach
	// its next select (and so cannot observe the trigger) without having
	// already finished ingesting every event dequeued so far.
	waitForChannelDrained(t, ch)
	fa.trigger()

	argvs := waitForArgvCount(t, logPath, 1)
	if len(argvs) != 1 {
		t.Fatalf("invocations = %d, want exactly 1 (storm collapse, not 5 separate notifications)", len(argvs))
	}
	joined := joinArgv(argvs[0])
	if !strings.Contains(joined, "wt-cockpit") || !strings.Contains(joined, "5 worktrees") {
		t.Errorf("argv %q, want it to mention wt-cockpit and 5 worktrees", argvs[0])
	}
}

// TestRunCooldownSuppressesRepeatedFireAcrossWindows: the same
// (worktree,rule,file) key tripping twice, with the second re-trip inside
// the cooldown, must produce exactly one stub invocation total, and must
// never even arm a second window.
func TestRunCooldownSuppressesRepeatedFireAcrossWindows(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	stubArgvLogger(t, dir, logPath)
	t.Setenv("PATH", dir)

	n := New(Config{Enabled: true, Severity: "danger", Cooldown: 10 * time.Minute}, noopLookup)
	fa := newFakeAfter()
	n.after = fa.after

	ch := make(chan model.Event, 8)
	stop := runNotifierForTest(n, ch)
	defer stop()

	hitEvent := model.Event{
		Type: model.EventGuardrail,
		ID:   "wt1",
		Hit:  &model.GuardrailHit{Rule: "secrets-pattern", File: "a.go", Severity: "danger", Message: "hit"},
	}

	ch <- hitEvent
	fa.waitArmed(t)
	fa.trigger()
	waitForArgvCount(t, logPath, 1)

	ch <- hitEvent // same key, well inside the 10m cooldown
	fa.assertNeverArms(t, 200*time.Millisecond)
	if got := len(readArgvLog(t, logPath)); got != 1 {
		t.Errorf("invocations = %d, want exactly 1 (the re-trip inside cooldown must be suppressed)", got)
	}
}

// TestRunSeverityFilterDropsWarnByDefault: a warn-severity hit never even
// arms the coalescing window (let alone fires) under the default "danger"
// floor.
func TestRunSeverityFilterDropsWarnByDefault(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	stubArgvLogger(t, dir, logPath)
	t.Setenv("PATH", dir)

	n := New(Config{Enabled: true, Severity: "danger"}, noopLookup)
	fa := newFakeAfter()
	n.after = fa.after

	ch := make(chan model.Event, 4)
	stop := runNotifierForTest(n, ch)
	defer stop()

	ch <- model.Event{Type: model.EventGuardrail, ID: "wt1", Hit: &model.GuardrailHit{Rule: "r", File: "f", Severity: "warn", Message: "m"}}
	fa.assertNeverArms(t, 200*time.Millisecond)
	if got := len(readArgvLog(t, logPath)); got != 0 {
		t.Errorf("invocations = %d, want 0 (a warn hit must not pass the default danger floor)", got)
	}
}

// TestRunBrokenNotifierExitNonZeroDoesNotCrashOrBlock: a stub that always
// exits non-zero must not crash the notifier or wedge the bus, and its
// failure log must be rate-limited (first occurrence only, out of two).
func TestRunBrokenNotifierExitNonZeroDoesNotCrashOrBlock(t *testing.T) {
	dir := t.TempDir()
	name := wantBinaryName(runtime.GOOS)
	if name == "" {
		t.Skipf("no notifier binary defined for GOOS %q", runtime.GOOS)
	}
	writeStub(t, dir, name, "#!/bin/sh\nexit 1\n")
	t.Setenv("PATH", dir)

	n := New(Config{Enabled: true, Severity: "danger"}, noopLookup)
	fa := newFakeAfter()
	n.after = fa.after

	ch := make(chan model.Event, 4)
	stop := runNotifierForTest(n, ch)

	logs := captureLogs(t, func() {
		ch <- model.Event{Type: model.EventGuardrail, ID: "wt1", Hit: &model.GuardrailHit{Rule: "r", File: "f", Severity: "danger", Message: "m"}}
		fa.waitArmed(t)
		fa.trigger()

		// A second, distinct window: proves Run keeps consuming normally after
		// a failed exec rather than getting stuck.
		ch <- model.Event{Type: model.EventGuardrail, ID: "wt2", Hit: &model.GuardrailHit{Rule: "r", File: "f", Severity: "danger", Message: "m"}}
		fa.waitArmed(t)
		fa.trigger()

		stop() // waits for Run to actually exit — proves it isn't wedged
	})

	if got := strings.Count(logs, "desktop notification exec failed"); got != 1 {
		t.Errorf("warn log occurrences = %d, want exactly 1 (rate-limited: first failure only, out of 2)", got)
	}
}

// prependToPATH puts dir first on PATH (so exec.LookPath resolves a stub
// named "osascript"/"notify-send" placed there) while leaving the rest of
// the real PATH intact — unlike t.Setenv("PATH", dir), which would ALSO
// break any external command (touch, sleep, sh itself resolving them) that
// the stub script's own body shells out to.
func prependToPATH(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// writeHangOnceStub writes an executable stub that HANGS (sleeps well past
// execTimeout) on its first invocation, then exits 0 immediately on every
// invocation after — distinguished via a marker file baked into the script,
// created before the hang starts so it survives the process being killed
// mid-sleep. "exec sleep" (not a backgrounded/forked sleep) replaces the
// script's own shell process in place, so a SIGKILL aimed at the single
// pid exec.CommandContext tracks reaches the actual sleeping process
// directly — no orphaned child left behind once the context times out.
func writeHangOnceStub(t *testing.T, dir string) {
	t.Helper()
	name := wantBinaryName(runtime.GOOS)
	if name == "" {
		t.Skipf("no notifier binary defined for GOOS %q", runtime.GOOS)
	}
	marker := filepath.Join(dir, "hung-once.marker")
	script := "#!/bin/sh\n" +
		"if [ -f \"" + marker + "\" ]; then\n" +
		"  exit 0\n" +
		"fi\n" +
		"touch \"" + marker + "\"\n" +
		"exec sleep 30\n"
	writeStub(t, dir, name, script)
}

// TestRunHungNotifierExecTimesOutAndDoesNotBlockNextEvent is the PROBE's
// "a notify exec that hangs doesn't block the bus/next events (timeout)"
// case: a stub that hangs well past execTimeout (5s) on its first
// invocation must be killed by fire's own context timeout (logged as
// exactly one failure), and Run's single worker goroutine must recover and
// process a second, later window normally afterwards rather than staying
// wedged.
func TestRunHungNotifierExecTimesOutAndDoesNotBlockNextEvent(t *testing.T) {
	dir := t.TempDir()
	writeHangOnceStub(t, dir)
	prependToPATH(t, dir)

	n := New(Config{Enabled: true, Severity: "danger"}, noopLookup)
	fa := newFakeAfter()
	n.after = fa.after

	ch := make(chan model.Event, 4)
	stop := runNotifierForTest(n, ch)

	logs := captureLogs(t, func() {
		ch <- model.Event{Type: model.EventGuardrail, ID: "wt1", Hit: &model.GuardrailHit{Rule: "r", File: "f", Severity: "danger", Message: "m"}}
		fa.waitArmed(t)
		fa.trigger() // fires the hanging exec; its own 5s context timeout must kill it

		// A SECOND, distinct window must still be processed normally once the
		// hang is killed — waitArmed blocking here (up to its own 10s budget)
		// until Run has looped back and re-armed is itself the "not wedged"
		// proof; a stuck Run would time this test out.
		ch <- model.Event{Type: model.EventGuardrail, ID: "wt2", Hit: &model.GuardrailHit{Rule: "r", File: "f", Severity: "danger", Message: "m"}}
		fa.waitArmed(t)
		fa.trigger()

		stop() // waits for Run to actually exit — proves it isn't wedged
	})

	if got := strings.Count(logs, "desktop notification exec failed"); got != 1 {
		t.Errorf("the hung exec should log exactly one failure (context deadline exceeded), got %d; logs=%s", got, logs)
	}
}

// TestRunShutdownWhileExecInFlightReturnsPromptlyNotWaitingFullTimeout is
// the PROBE's "shutdown mid-flight, race-clean" case: cancelling Run's ctx
// while fire() is blocked inside a hung exec must cut that exec short via
// the LINKED execCtx (context.WithTimeout(ctx, execTimeout) in fire), so Run
// returns promptly — not after waiting out the full 5s execTimeout on its
// own. Uses an ALWAYS-hanging stub (not writeHangOnceStub) since there is
// only ever one invocation here.
func TestRunShutdownWhileExecInFlightReturnsPromptlyNotWaitingFullTimeout(t *testing.T) {
	dir := t.TempDir()
	name := wantBinaryName(runtime.GOOS)
	if name == "" {
		t.Skipf("no notifier binary defined for GOOS %q", runtime.GOOS)
	}
	writeStub(t, dir, name, "#!/bin/sh\nexec sleep 30\n")
	prependToPATH(t, dir)

	n := New(Config{Enabled: true, Severity: "danger"}, noopLookup)
	fa := newFakeAfter()
	n.after = fa.after

	ch := make(chan model.Event, 4)
	stop := runNotifierForTest(n, ch)

	ch <- model.Event{Type: model.EventGuardrail, ID: "wt1", Hit: &model.GuardrailHit{Rule: "r", File: "f", Severity: "danger", Message: "m"}}
	fa.waitArmed(t)
	fa.trigger() // Run is now blocked inside fire()'s cmd.Run(), executing the always-hanging stub

	// Let fire() actually start the exec before cancelling, so this genuinely
	// exercises "cancel while in flight" rather than racing cancel() ahead of
	// flushAndFire even beginning.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("Run did not return within 4s of ctx cancellation while an exec was in flight (want well under the 5s exec timeout)")
	}
	if elapsed := time.Since(start); elapsed >= execTimeout {
		t.Errorf("Run took %v to return after cancellation (>= the %v exec timeout) — shutdown should cut a hung exec short via the linked context, not merely wait for its own natural timeout", elapsed, execTimeout)
	}
}

// TestRunStopsCleanlyUnderConcurrentEventsNoLeak drives events concurrently
// with shutdown using the REAL clock/timer (no fake), specifically to
// exercise the shutdown path under -race: ctx cancellation racing incoming
// bus events and a live 5s window timer must never panic, deadlock, or
// leave Run running after stop() returns.
func TestRunStopsCleanlyUnderConcurrentEventsNoLeak(t *testing.T) {
	dir := t.TempDir()
	name := wantBinaryName(runtime.GOOS)
	if name == "" {
		t.Skipf("no notifier binary defined for GOOS %q", runtime.GOOS)
	}
	writeStub(t, dir, name, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir)

	n := New(Config{Enabled: true, Severity: "danger"}, noopLookup)
	ch := make(chan model.Event, 64)
	stop := runNotifierForTest(n, ch)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			select {
			case ch <- model.Event{Type: model.EventGuardrail, ID: "wt1", Hit: &model.GuardrailHit{Rule: "r", File: "f", Severity: "danger", Message: "m"}}:
			default:
			}
		}
	}()
	stop() // cancel + block until Run has actually returned
	wg.Wait()
}

// captureLogs redirects the default slog logger for the duration of fn and
// returns everything logged as text.
func captureLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}
