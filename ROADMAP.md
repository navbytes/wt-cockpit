# Roadmap — wt cockpit: MVP → polished

The product thesis stays fixed at every phase: **a read-only, agent-agnostic diff
cockpit** for many worktrees across many projects. It never launches or controls
agents. Every phase below is shippable on its own; none repaints what an earlier
phase built, because the engine/frontend split and the swappable interfaces
(`GitBackend`, `Watcher`, `Store`) were designed for exactly this sequence.

Versioning: semver, `v0.x` until the protocol freezes at `v1.0`. The daemon↔client
protocol carries a version in its handshake from v0.2 on; minor versions may add
fields, only a major may remove them.

---

## v0.1 — Engine MVP ✅ (this release)

What exists today, built test-first (39 tests, `-race` clean, pure stdlib):

- `wtd` daemon: discovery across roots, per-worktree diff vs merge-base (committed
  + uncommitted + untracked), guardrail rules, review store, event bus, HTTP+SSE
  over a Unix socket (optional TCP).
- `wt` client: `ls` radar, `watch` (live SSE), `diff`, `review`, `refresh`, and the
  gated `approve` (full review + clean tree → merge into base, abort-safe on
  conflict, worktree removed).
- Polling watcher (2s default) as the change driver.

## v0.2 — Live & robust (the daemon earns trust)

Goal: the daemon becomes something you leave running for weeks.

- ✅ **fsnotify watcher** behind the existing `Watcher` interface: git-state-first
  (`.git/HEAD`, `.git/index`, refs) + `.gitignore`-aware working-tree watches;
  debounce (~150ms); bounded re-diff worker pool. Polling stays as the fallback
  for network mounts.
- ✅ **Config file** `~/.config/wtcockpit/config.toml`: roots, per-repo base branch,
  guardrail rules (the engine already treats rules as data), activity window.
- ✅ **Review identity fix**: key review state per-file on (path, content hash) rather
  than whole-diff hash, so committing work or touching one file no longer resets
  review on the others.
- **Protocol handshake** with `protocolVersion`; clients refuse a major mismatch.
- **Service files**: launchd plist (macOS) + systemd unit (Linux), `wtd install`.
- Structured logging, `wt status` (daemon health, watch counts, scan timings).

## v0.3 — The TUI (daily driver) ✅ (shipped)

Goal: replace the ANSI `ls`/`watch` with the full-screen cockpit from the UX mock.
Shipped: radar + virtualized diff pane, chroma highlighting, review/approve flow,
tmux jump, TTY-default `wt`; unified view in-TUI (side-by-side belongs to v0.4's
reading room). Deferred within phase: `c` comments key (v0.4), in-diff search.

- **Bubble Tea** radar: project→worktree tree pane + live diff pane, pulse states,
  guardrail badges; `viewport`-virtualized rendering so 5k-line diffs never block.
- **Chroma** syntax highlighting; big/generated files collapsed by default.
- Review flow in-TUI: per-file reviewed toggles, progress bar, `a` to approve with
  a confirm dialog showing the gates.
- `t` jump-to-tmux (focus the pane/window whose cwd is the worktree).
- Keybindings exactly as mocked: `↑↓/jk`, `⏎`, `r`, `/` search, `f` filter-active,
  `esc`, `q`.

## v0.4 — The reading room (web)

Goal: deliberate side-by-side review when a diff deserves a bigger screen.

- Embedded web UI (`embed.FS`, no Node at runtime): side-by-side per-hunk view,
  file checklist rail, reviewed progress, approve button — the v2 of the HTML mock,
  now live against the daemon's SSE stream.
- **Comments for agents**: inline comments persist via the existing store; `wt
  comments <id>` and a `--json` mode so an agent (Claude Code, Aider) can consume
  review feedback and iterate. This closes the review→agent loop.
- `wt open <id>` opens the browser to that worktree's review page.

## v0.5 — Guardrails+ & notifications

Goal: "catch mistakes fast" without watching the screen.

- ✅ Rule pack: secrets-shaped strings (entropy + patterns), lockfile/snapshot churn,
  file-count and total-churn thresholds, protected-path deletes, new-dependency
  detection (go.mod/package.json/Cargo.toml deltas).
- ✅ Per-repo rule overrides (`.wtcockpit.toml` in-repo, checked in by the team).
- ✅ Notifications: terminal bell/OSC in `wt watch`, `notify-send`/`osascript`
  desktop notifications from the daemon on danger-severity hits; a matching
  transient toast in the web reading room.
- ✅ **Menu-bar companion**: `wt menubar` emits SwiftBar/xbar plugin text (danger
  count + reviewed/total files, per-worktree rows, click-through to the reading
  room) — a thin emitter against the existing API rather than a native app
  (avoids a cgo/systray dependency for a status badge).

## v0.6 — Performance & scale (monorepo-grade)

Goal: instant at 100+ worktrees and monorepo-size diffs.

- **SQLite store** behind `Store` (modernc.org/sqlite, still a static binary);
  migration from the JSON file on first run.
- Benchmark suite + profiles as CI artifacts; budgets: <50ms radar refresh with
  100 worktrees, <1s re-diff p95 on a 1M-file monorepo (git CLI).
- Evaluate **gitoxide sidecar / libgit2** behind `GitBackend` for status/diff hot
  paths; adopt only if profiles justify it.
- Diff pagination in the API (hunk ranges) so huge files stream on demand.

## v0.7 — Remote & fleet

Goal: glance at your agents from anywhere.

- Token auth + TLS option for TCP; first-class Tailscale docs (`wtd -tcp
  tailscale-ip:7799`).
- Mobile-friendly reading room layout (the phone glance while away from the desk).
- `wt --host` to point the client at a remote daemon; multiple daemons aggregated
  in one radar (workstation + build box).

## v1.0 — Polish & freeze

Goal: something you'd tell a stranger to install.

- Protocol v1 frozen; compatibility promise documented.
- Packaging: `go install`, Homebrew tap, prebuilt release binaries via goreleaser,
  signed macOS builds.
- Docs site: quickstart, config reference, guardrail cookbook, agent-integration
  guide (how to have your agent read comments), demo GIF (vhs).
- Onboarding: `wt init` interactive setup; sensible zero-config default (scan
  `~/code`, default rules).
- Hardening pass: fuzz the diff parser, chaos-test the watcher, soak test.

---

## Cross-cutting principles (every phase)

- Test-first stays the norm; the race detector stays in CI.
- The daemon never gains a second write path; `approve` remains the only mutation
  and keeps its gates.
- Everything ships as a single static binary per platform.
- No telemetry. It's a tool that reads your code; it phones no one.

## Deliberate non-goals

Launching/managing agents (Conductor et al. own that), prompt management, team/PM
boards, PR/code-host integration (the cockpit reviews *pre-merge worktrees*; once
merged, your normal PR flow applies), Windows-native watcher before v1.0 (WSL2
works throughout).
