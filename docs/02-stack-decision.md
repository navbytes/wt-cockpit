# wt cockpit — Stack & Architecture (built so it won't become a bottleneck)

The MVP is small. What determines whether you regret it in a year isn't the TUI library — it's a handful of structural decisions made now. This doc names the future bottlenecks explicitly and picks a stack that designs them out, while keeping the language choice *reversible*.

---

## 0. The one decision that prevents most future pain

**Separate the engine from the UI. The TUI is a client, not the app.**

Everything you might want later — the web "reading room," a menu-bar app, a remote/mobile view over Tailscale, scripting/automation — dies the moment your git/watch/diff logic is entangled with Bubble Tea's `Model`. So structure it as three layers from commit #1:

```
┌─────────────────────────────────────────────────────────────┐
│  FRONTENDS (thin clients — hold no git logic)                │
│   • TUI (Bubble Tea)   • local web UI   • menu-bar (later)    │
└───────────────▲──────────────────────────┬──────────────────┘
                │  events (SSE/WS stream)   │  commands (RPC)
┌───────────────┴──────────────────────────▼──────────────────┐
│  wtd — the daemon / engine (headless, long-lived)            │
│   • repo+worktree registry     • guardrail rule engine       │
│   • watcher (fs + git-state)   • diff service                │
│   • review/comment store       • event bus                   │
└───────────────┬──────────────────────────────────────────────┘
                │  GitBackend interface  (swappable)
┌───────────────▼──────────────────────────────────────────────┐
│  git access:  git CLI (v1)  →  libgit2 / gitoxide (later)    │
└──────────────────────────────────────────────────────────────┘
```

Why this specific shape is the anti-bottleneck:

- **One engine, many faces.** The web UI and menu-bar become ~200-line clients that subscribe to the same event stream. You never re-implement diffing per frontend.
- **The daemon is where "live" lives.** Watching, debouncing, and diff caching happen once, in a long-lived process, instead of being re-derived every time a UI repaints.
- **Language becomes reversible.** Because frontends talk to `wtd` over a versioned protocol, you can rewrite the *engine* in another language later without touching the UIs — or vice versa. That's what makes the Go-vs-Rust choice below low-stakes rather than a fork in the road.
- **`GitBackend` interface** means "shell out to git" today and "gitoxide/libgit2" tomorrow is a contained swap, not a migration.

Keep the protocol boring and versioned: **JSON-RPC for commands + Server-Sent Events for the live push**, both over a Unix domain socket (localhost HTTP is the fallback for remote/web). SSE (not WebSocket) because the traffic is overwhelmingly engine→UI streaming; commands are infrequent and fine as plain request/response. Put a `protocolVersion` in the handshake on day one.

---

## 1. Language: pick Go for the MVP, and here's exactly when to pick Rust instead

Both give you a single static binary, great watchers, and mature TUIs. The honest 2026 picture:

| | **Go + Bubble Tea** | **Rust + Ratatui** |
|---|---|---|
| Ship speed / iteration | **Faster** — Elm-style `Model/Update/View`, batteries-included (Bubbles, Lipgloss) | Slower — you hand-roll the event loop + state; borrow checker tax on async+TUI |
| Concurrency model for "watch N repos → fan out to M clients" | **Goroutines + channels — this app's exact sweet spot** | tokio + channels; powerful but more ceremony |
| Embedded web server + SSE + embedded assets | **stdlib `net/http` + `embed.FS`, trivial** | axum + rust-embed, also fine |
| Git library ceiling | go-git is **slow on big repos** (well-documented) → you'll shell out to git | **gitoxide (`gix`) is the fastest option going**, and it's read-heavy — exactly your workload |
| Runtime perf / memory on high-frequency updates | Good; GC pauses irrelevant at this scale | ~30–40% less memory, ~15% less CPU on 1k-updates/sec dashboards |

**Recommendation: build the MVP in Go.** Your dominant early risk is iterating on UX and getting "live" to feel instant — that's velocity, and Go+Bubble Tea wins velocity decisively. The performance gap doesn't bite at MVP sizes, and the daemon architecture means if a hot path (or the whole engine) ever needs Rust, that's a bounded rewrite behind a stable protocol, not a restart.

**Choose Rust from day one only if one of these is already true:** (a) your repos are large monorepos where **diff/status latency is the core feature risk** — then gitoxide's read performance is a feature, not an optimization; (b) you're simply more fluent in Rust and will ship faster in it; or (c) you know you want a **native macOS core** the menu-bar app links directly, sharing a Rust library rather than talking to a daemon.

> Note the asymmetry that makes Go safe here: the thing Rust is best at (fast git reads) is exactly the thing hidden behind `GitBackend`. So you get Go's velocity now and Rust's ceiling later *in the one place it matters*, without committing to Rust everywhere.

---

## 2. Concrete library picks

### Go stack (recommended MVP)

| Layer | Pick | Why / bottleneck note |
|---|---|---|
| TUI | **Bubble Tea** + **Lipgloss** (style) + **Bubbles** (list, viewport, textinput) | `viewport` gives you virtualized scrolling for big diffs for free |
| Terminal syntax highlight | **Chroma** (`alecthomas/chroma`) | pure Go, no external binary; lexers for all your langs |
| Terminal diff | shell `git diff` → parse; render hunks yourself. Optionally vendor **delta**'s look, don't depend on the binary | keeps you correct with the user's git config/ignore rules |
| Git backend (v1) | **os/exec → git CLI** behind a `GitBackend` interface | correct by construction (honors `.gitignore`, hooks, config); the interface is the escape hatch |
| Git backend (later) | **libgit2** via `git2go`, or call a small **gitoxide** sidecar | only if process-spawn shows up in profiles |
| File watching | **fsnotify** for the common case; **`rjeczalik/notify`** if you need recursive FSEvents/inotify | see §3 — the watching *strategy* matters far more than the library |
| Web server (reading room) | stdlib **`net/http`** + **SSE**; assets via **`embed.FS`** | single binary still; no Node runtime shipped |
| IPC (TUI ↔ daemon) | JSON-RPC over **Unix socket** (`net.Listen("unix", …)`) | localhost HTTP fallback enables remote/Tailscale later |
| Persistence | **SQLite** via **`modernc.org/sqlite`** (pure Go, no CGO) | keeps the static-binary property; stores review state, comments, guardrail config, per-worktree "seen" cursors |
| Config / guardrail rules | **TOML** (`BurntSushi/toml`) | declarative rules (globs, thresholds) so guardrails are data, not code |
| Web-side diff render | **diff2html** (light) or **Monaco diff editor** (rich, if you want inline comments/editing) | only loaded in the reading room, not the hot path |

### Rust stack (if you go that way)

Ratatui + crossterm · **gitoxide (`gix`)** for read ops (status/diff/refs/tree — its strengths, and 100% of your read path), shell out to `git` for the merge/worktree-remove writes · **`similar`** for diffing + **`syntect`** for highlight · **`notify`** for watching · **axum** + SSE + **rust-embed** for the web server · **tokio** · **rusqlite** for persistence · **tonic** (gRPC) or axum-over-UDS for IPC.

---

## 3. The actual future bottlenecks, and how the design pre-empts each

This is the part that matters. Naming them now is cheaper than hitting them later.

**1. File-watch exhaustion (the #1 real risk).** Recursively watching many working trees blows past Linux inotify limits (`max_user_watches`/`max_user_instances`) and drowns you in `node_modules`/`target`/`.venv` churn. *Design:* don't naively recurse. Make **git state the primary signal** — watch each worktree's `.git/index`, `.git/HEAD`, `.git/logs/HEAD` (tiny, authoritative for commits/staging/branch switches). Watch the working dir for *uncommitted* edits but **filter through `.gitignore`** and never descend into ignored dirs. On macOS, FSEvents is directory-coalesced and cheap; on Linux, the git-state-first approach keeps watch counts in the dozens, not thousands.

**2. `git` process-spawn storms.** A "live" tool that blindly polls `git diff` across 30 worktrees every second is a fork bomb in slow motion. *Design:* be **event-driven, not poll-driven** — only re-diff a worktree when its watcher fires. **Debounce** (coalesce a burst of saves into one diff after ~150ms quiet). Run diffs through a **bounded worker pool** (e.g. `GOMAXPROCS`-sized) so 30 simultaneous changes don't spawn 30 gits. **Cache** the last diff per worktree keyed on `index` mtime + `HEAD`; serve cached bytes to any number of clients. Keep polling only as a slow safety-net fallback (every ~10s) for filesystems where events are unreliable (some network mounts).

**3. Large diffs freezing the UI.** A 5,000-line agent diff must never block the render loop. *Design:* compute diffs **off the UI goroutine** and stream hunks to the frontend. **Virtualize** the rendered list (Bubble Tea's `viewport` / Ratatui's stateful list render only what's visible). **Cap** eagerly-rendered lines per file (e.g. 400) with an explicit "load more" — huge generated files (lockfiles, snapshots) shouldn't be rendered at all by default; collapse them.

**4. Many worktrees / many projects.** *Design:* the registry is a **flat indexed map**, updates are **incremental deltas** on the event bus (not full-state resends), and the sidebar list is virtualized. This scales to hundreds of worktrees because a UI repaint touches only the changed rows.

**5. The reading room needing rich review UX later** (inline comments, threads, "agent, fix this"). *Design:* persist review artifacts in **SQLite** now — `reviewed(worktree_id, file, at)`, `comment(worktree_id, file, line, body, state)` — even if the MVP only toggles a checkbox. Retrofitting a comment model onto a stateless UI later is painful; a table you're barely using now is free.

**6. Guardrails growing from 2 rules to 20.** *Design:* guardrails are **declarative config** (globs + thresholds + severity) evaluated by one rule engine over each diff, not `if` statements sprinkled in the UI. Adding "warn on edits to `.github/workflows`" becomes a config line.

**7. Cross-platform + remote (Linux dev box, macOS laptop, phone over Tailscale).** *Design:* keep OS-specific watch code behind the watcher interface; keep the daemon's transport as localhost HTTP-capable (not *only* a Unix socket) so binding it to a Tailscale interface later is a config flag, not an architecture change. This is why the earlier plan favored TUI+web over a native Swift app — the daemon is inherently network-addressable.

---

## 4. Data & protocol specifics worth pinning now

- **Diff baseline default:** "everything this worktree changed since it forked from `main`" = `git diff --merge-base main` (three-dot semantics) for committed work, plus working-tree changes overlaid. Make the base configurable per worktree; store it.
- **Event bus payloads:** small, typed deltas — `worktree.updated{id, stats, state}`, `diff.ready{id, hash}`, `guardrail.tripped{id, rule}`. Never ship full diffs on the bus; ship a hash and let the client pull the diff by id (cacheable, paginated).
- **Protocol versioning:** `hello{protocolVersion}` handshake; refuse mismatched majors with a clear message. Costs nothing now, saves a painful flag day when the web UI and daemon drift.
- **Single-flight discovery:** repo/worktree discovery (`git worktree list` across roots) runs on a schedule *and* on filesystem events for the root dirs, deduped so a burst doesn't re-scan repeatedly.

---

## 5. Build order (each phase ships something usable, none paints you in)

1. **Engine skeleton + `GitBackend` (CLI) + registry + JSON over Unix socket.** No UI yet; a `wt ls` subcommand proves the pipe.
2. **Watcher (git-state-first) + debounce + diff cache + event bus.** Now the daemon is "live."
3. **Bubble Tea TUI as a pure client** — the radar you mocked, subscribing to events. This is your daily driver.
4. **SQLite review store + guardrail rule engine (declarative).** Reviewed toggles, comments, alerts persist.
5. **Reading room:** `net/http` + SSE + embedded web assets, `r` opens the browser. Reuses the exact diff service.
6. **Approve & merge action** (shell out to git; fast-forward + optional worktree remove) — the only *write* path, gated on full review.
7. *(optional)* menu-bar client + bind daemon to Tailscale for remote/phone viewing.

---

## 6. "Won't-regret-it" checklist

- [ ] Git access is behind an interface; the UI never shells out to git directly.
- [ ] Watching is git-state-first and `.gitignore`-aware; no recursive watch of ignored dirs.
- [ ] Diffs are event-driven, debounced, worker-pooled, and cached — never blind-polled.
- [ ] The engine is a daemon; every frontend is a thin client on a versioned protocol.
- [ ] The event bus ships deltas + hashes, not full state or full diffs.
- [ ] Review state and comments live in SQLite from day one, even if barely used.
- [ ] Guardrails are declarative config evaluated by one rule engine.
- [ ] The daemon can bind to a network interface (not only a Unix socket) for future remote use.
- [ ] Big/generated files are collapsed by default; rendered lists are virtualized.

---

### Bottom line

Build the MVP in **Go + Bubble Tea**, but spend your first commits on the **daemon/engine split, the `GitBackend` interface, and a git-state-first event-driven watcher** — those three are what keep it from ever becoming a bottleneck. The language, the diff library, even the whole engine stay swappable because the frontends only ever speak a small versioned protocol. If you already know you're living in giant monorepos, start the engine in **Rust + gitoxide** instead; the architecture above is identical either way.
