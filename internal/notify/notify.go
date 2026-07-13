package notify

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// Lookup resolves a worktree id to its owning repo and worktree/branch name,
// for notification titles. cmd/wtd wires this to a closure over the
// registry (reg.Get) — this package never imports the engine or the
// registry directly (P5-design.md §1.5).
type Lookup func(id string) (repo, name string, ok bool)

// Config is [notifications]'s validated, defaulted runtime shape.
// internal/config.NotificationsConfig decodes and defaults the TOML; this is
// just what New needs, so this package never imports internal/config.
type Config struct {
	Enabled  bool
	Severity string        // "danger" (default floor) | "warn" (admits warn+danger too)
	Cooldown time.Duration // per-(worktree,rule,file) re-notify suppression; <=0 -> 10m default
}

// binKind names which platform notifier binary a Notifier resolved to, or
// why it has none — the source for statusPayload's additive `notifier`
// field (P5-design.md §1.5, §2).
type binKind string

const (
	binOsascript   binKind = "osascript"
	binNotifySend  binKind = "notify-send"
	binDisabled    binKind = "disabled (config)"
	binUnavailable binKind = "unavailable (no notifier binary)"
)

// execTimeout bounds a single notifier exec (P5-design.md §1.5's "Execution
// discipline") — a hung osascript/notify-send must not wedge the notifier.
const execTimeout = 5 * time.Second

// Notifier is a registry-bus subscriber that fires an OS desktop
// notification on guardrail.tripped events passing its severity floor, via
// a fixed-argv exec of the resolved platform binary — never a shell.
// Construct with New; drive it with Run.
type Notifier struct {
	cfg     Config
	lookup  Lookup
	bin     binKind
	binPath string

	coalesce *coalescer
	after    func(time.Duration) <-chan time.Time // window-timer indirection; time.After in production, faked in tests

	failCount atomic.Int64 // consecutive exec failures — rate-limits the warn log
}

// New builds a Notifier for production use (real clock, real time.After).
// The notifier binary is probed for exactly once here (see probe); Run never
// touches PATH again.
func New(cfg Config, lookup Lookup) *Notifier {
	return newNotifier(cfg, lookup, time.Now, time.After)
}

// newNotifier is New's testable core: now/after are the "timer indirection"
// P5-design.md §1.5 point 4 calls for, so coalescing (the 5s window
// especially) is exercisable deterministically, with no real sleeps.
func newNotifier(cfg Config, lookup Lookup, now func() time.Time, after func(time.Duration) <-chan time.Time) *Notifier {
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = defaultCooldown
	}
	bin, path := probe(cfg.Enabled, runtime.GOOS)
	return &Notifier{
		cfg:      cfg,
		lookup:   lookup,
		bin:      bin,
		binPath:  path,
		coalesce: newCoalescer(cfg.Cooldown, now),
		after:    after,
	}
}

// wantBinaryName is the notifier binary each GOOS uses, or "" for anything
// else (P5-design.md §1.5: "others -> disabled"). A parameter, not a bare
// runtime.GOOS reference, so platform selection is unit-testable regardless
// of which OS actually runs the test — the same seam cmd/wt's
// browserCommand(goos) already uses, for the identical reason.
func wantBinaryName(goos string) string {
	switch goos {
	case "darwin":
		return "osascript"
	case "linux":
		return "notify-send"
	default:
		return ""
	}
}

// probe resolves which notifier binary (if any) is available. Disabled by
// config short-circuits before ever touching PATH — an operator who turned
// notifications off shouldn't pay LookPath's cost or see an "unavailable"
// log that doesn't apply. Otherwise exec.LookPath is tried once for this
// GOOS's binary: a deliberate, documented trade (P5-design.md §1.5) — an
// absolute path would be marginally more hijack-resistant, but LookPath is
// exactly what lets a test (or an operator's own troubleshooting) put a
// stand-in notifier anywhere on PATH.
func probe(enabled bool, goos string) (binKind, string) {
	if !enabled {
		return binDisabled, ""
	}
	name := wantBinaryName(goos)
	if name == "" {
		return binUnavailable, ""
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return binUnavailable, ""
	}
	if name == "osascript" {
		return binOsascript, path
	}
	return binNotifySend, path
}

// Status reports the notifier's resolved binary state as a short, stable
// string — statusPayload's additive `notifier` field.
func (n *Notifier) Status() string { return string(n.bin) }

// active reports whether this Notifier can ever actually fire.
func (n *Notifier) active() bool { return n.bin == binOsascript || n.bin == binNotifySend }

// Run drains ch — the caller's own registry.Subscribe(...) channel, e.g.
// `ch, cancel := reg.Subscribe(256)` — until ctx is cancelled or ch closes,
// applying the severity filter, the coalescer, and (on window flush) firing
// the resulting OS notification(s). It is the notifier's ONE worker
// goroutine: every exec happens serially, right here, which is what gives
// "one exec at a time" its natural global rate limit (P5-design.md §1.5's
// "Execution discipline") — further bus events simply queue in ch's own
// buffer while an exec is in flight (and are dropped if it fills, same as
// any other slow registry subscriber).
//
// When no notifier binary is available (a LookPath miss, or an unsupported
// GOOS), Run logs exactly once at Info and returns immediately without ever
// reading ch — the caller unsubscribes (see cmd/wtd's wiring) and the bus
// keeps flowing untouched for every other subscriber. cfg.Enabled=false
// never reaches here in production (cmd/wtd only subscribes+Runs when
// enabled — "skips the subscription entirely"), but Run is a safe no-op
// either way, logging nothing in that case (there is nothing wrong to
// report).
func (n *Notifier) Run(ctx context.Context, ch <-chan model.Event) {
	if !n.active() {
		if n.cfg.Enabled {
			name := wantBinaryName(runtime.GOOS)
			if name == "" {
				slog.Info("desktop notifications unavailable: unsupported platform", "goos", runtime.GOOS)
			} else {
				slog.Info("desktop notifications unavailable: notifier binary not found in PATH", "binary", name)
			}
		}
		return
	}

	var windowC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if n.ingest(e) && windowC == nil {
				windowC = n.after(coalesceWindow)
			}
		case <-windowC:
			windowC = nil
			n.flushAndFire(ctx)
		}
	}
}

// ingest applies the severity filter (default: danger only; "warn" admits
// warn+danger too) and, on a pass, folds the hit into the coalescer.
// Anything that isn't a guardrail.tripped event is ignored outright.
func (n *Notifier) ingest(e model.Event) bool {
	if e.Type != model.EventGuardrail || e.Hit == nil {
		return false
	}
	if !n.passesSeverity(e.Hit.Severity) {
		return false
	}
	return n.coalesce.accept(e.ID, *e.Hit)
}

func (n *Notifier) passesSeverity(sev string) bool {
	if n.cfg.Severity == "warn" {
		return sev == "warn" || sev == "danger"
	}
	return sev == "danger"
}

// flushAndFire turns one coalescer flush into actual OS notification(s):
// either one exec per pending worktree group, or — storm collapse — a
// single summary exec (P5-design.md §1.5 points 2-3).
func (n *Notifier) flushAndFire(ctx context.Context) {
	r := n.coalesce.flush()
	if len(r.Storm) > 0 {
		n.fire(ctx, "wt-cockpit", n.stormBody(r.Storm), false)
		return
	}
	for _, g := range r.Groups {
		title := "wt-cockpit — " + n.label(g.worktreeID)
		body := g.first.Message
		if g.count > 1 {
			body = fmt.Sprintf("%s …and %d more", body, g.count-1)
		}
		n.fire(ctx, title, body, g.first.Severity == "danger")
	}
}

// label resolves a worktree id to "repo/name" via Lookup, falling back to
// the bare id when the worktree has since vanished from the registry (e.g.
// approved/removed between the hit landing and the window flushing).
func (n *Notifier) label(id string) string {
	if repo, name, ok := n.lookup(id); ok {
		return repo + "/" + name
	}
	return id
}

// stormBody builds the ">3 worktrees" summary body: "guardrail hits in K
// worktrees: name1, name2, …". Sanitize's own truncation (see fire) is what
// elides the list once it runs past the body cap, so no separate "+N more"
// bookkeeping is needed here.
func (n *Notifier) stormBody(ids []string) string {
	labels := make([]string, len(ids))
	for i, id := range ids {
		labels[i] = n.label(id)
	}
	return fmt.Sprintf("guardrail hits in %d worktrees: %s", len(ids), strings.Join(labels, ", "))
}

// fire sanitizes title/body (P5-design.md §1.5: control bytes are stripped
// before they ever become an argv element — belt and braces on top of the
// non-echo invariant, since a rule pack's own Message text is
// attacker/team-controlled content via a repo's .wtcockpit.toml, not just
// tracked file bytes), caps them (80/200 runes), builds the resolved
// binary's fixed argv, and execs it with a 5s timeout and no shell, ever.
// critical selects notify-send's --urgency (darwin's constant script has no
// urgency slot, so it's ignored there). A non-zero exit or exec failure is
// logged at warn for the first occurrence then every 100th (a broken
// notifier must not flood logs) and otherwise swallowed: a failed
// notification must never crash or block the bus.
func (n *Notifier) fire(ctx context.Context, rawTitle, rawBody string, critical bool) {
	title := Sanitize(rawTitle, titleCap)
	body := Sanitize(rawBody, bodyCap)

	var argv []string
	switch n.bin {
	case binOsascript:
		argv = buildArgvDarwin(title, body)
	case binNotifySend:
		urgency := "normal"
		if critical {
			urgency = "critical"
		}
		argv = buildArgvLinux(title, body, urgency)
	default:
		return
	}

	execCtx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	cmd := exec.CommandContext(execCtx, argv[0], argv[1:]...)
	if err := cmd.Run(); err != nil {
		n.logFailure(err)
		return
	}
	n.failCount.Store(0)
}

func (n *Notifier) logFailure(err error) {
	c := n.failCount.Add(1)
	if c == 1 || c%100 == 0 {
		slog.Warn("desktop notification exec failed", "error", err, "count", c)
	}
}
