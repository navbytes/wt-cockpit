# wt cockpit

[![CI](https://github.com/navbytes/wt-cockpit/actions/workflows/ci.yml/badge.svg)](https://github.com/navbytes/wt-cockpit/actions/workflows/ci.yml)
[![Go 1.24+](https://img.shields.io/badge/go-1.24+-00ADD8?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

A read-only, agent-agnostic **diff cockpit**: watch, catch mistakes in, and review the
diffs of many git worktrees across many projects while AI agents (Claude Code, Codex,
Aider, or plain git) work in parallel. It never launches or controls agents — it only
reads git and the filesystem, which is exactly what lets one tool span every setup at once.

![radar](docs/ux/radar.png)

This is the v0.1 engine, daemon, and terminal client, built test-first. See
[ROADMAP.md](ROADMAP.md) for the path to the full TUI, web reading room, and beyond,
and [docs/](docs/README.md) for the design history (why a viewer, why a daemon, why Go).

## Architecture

The engine is a headless daemon (`wtd`); every frontend is a thin client over a small
HTTP API on a Unix socket. This is the anti-bottleneck decision: the TUI, a future web
"reading room", and a menu-bar app are all clients of the same engine, and the language
or git library can be swapped without touching them.

```
frontends (thin clients)          wt (CLI/radar) · web · menu-bar
        │  events (SSE)  │  commands (JSON/HTTP)
        ▼                ▼
wtd — daemon/engine     discovery · registry+bus · diff · guardrails · review store
        │  GitBackend interface (swappable)
        ▼
git access              git CLI now  →  libgit2 / gitoxide later
```

Everything the design put behind an interface has a stdlib implementation now and a
richer drop-in later:

| Concern | Interface | MVP impl | Future drop-in |
|---|---|---|---|
| git access | `gitbackend.Backend` | shell out to `git` | libgit2 / gitoxide |
| change detection | `watcher.Watcher` | `Poller` (periodic) | fsnotify, git-state-first |
| persistence | `store.Store` | JSON file | SQLite |
| frontend | HTTP/SSE client | `wt` ANSI radar | Bubble Tea TUI, web |

## Packages

- `internal/model` — transport-agnostic domain types.
- `internal/diffparse` — pure unified-diff → structured hunks/lines parser.
- `internal/guardrail` — declarative glob + threshold rule engine (+ `**` globber).
- `internal/gitbackend` — `Backend` interface + git-CLI impl (incl. untracked files).
- `internal/discovery` — scan roots for repos (skips linked worktrees + heavy dirs).
- `internal/store` — review/comment persistence (JSON).
- `internal/registry` — in-memory state + pub/sub event bus, emits deltas not full state.
- `internal/watcher` — refresh driver (`Poller`).
- `internal/engine` — orchestration + query/command surface.
- `cmd/wtd` — daemon: engine + HTTP/SSE over a Unix socket (+ optional TCP).
- `cmd/wt` — terminal client: `ls`, `watch`, `diff`, `review`, `approve`, `refresh`.

## Install & run

```sh
go install github.com/navbytes/wt-cockpit/cmd/wtd@latest
go install github.com/navbytes/wt-cockpit/cmd/wt@latest
# or from a checkout:
make build   # → bin/wtd, bin/wt

# start the daemon over one or more roots (uses fsnotify watcher by default)
wtd -root ~/code -interval 1s &

# the client (defaults to ~/.wtcockpit/wtd.sock; override with WTD_SOCKET)
wt ls                       # radar: all worktrees, most-recently-changed first
wt watch                    # live radar, re-renders on every change (SSE)
wt diff <id>                # a worktree's structured diff
wt review <id> <file>       # mark a file reviewed (--off to unmark)
wt approve <id>             # merge worktree→base & remove it (gated)
wt refresh                  # force a rescan
```

`wtd -tcp 127.0.0.1:7799` additionally serves the same API over TCP (bind to a Tailscale
interface for remote/phone viewing).

## Configuration

Config file `~/.config/wtcockpit/config.toml` (or `$XDG_CONFIG_HOME/wtcockpit/config.toml`)
defines roots, per-repo base branch overrides, guardrail rules, and daemon options.
Precedence: explicit flags > config file > built-in defaults. Malformed TOML is a fatal error.

Example:
```toml
roots = ["~/code", "~/work"]
base = "main"
watch = "fsnotify"          # or "poll" to disable file watching
interval = "2s"

[repos."/home/user/code/api"]
base = "develop"            # per-repo override

[[rules]]
name = "example-rule"
severity = "warn"
path_glob = "**/*.yaml"
```

See [docs/config.example.toml](docs/config.example.toml) for the full reference.
Use `-config /path/to/config.toml` to specify a custom location, or `-watch fsnotify|poll`
to override the watcher backend at runtime.

## What the engine does each refresh

Discover repos under the roots → expand worktrees via git → for each, diff against its
base (merge-base of `base..HEAD` through the working tree, **including untracked files**)
→ parse into structured hunks → evaluate guardrails → compute state (active/dirty/idle)
→ upsert into the registry, which emits a delta only when something actually changed.
Review state resets automatically when a worktree's diff content changes.

## Approve & merge (the write path)

`Approve` is the only mutating operation, and it is deliberately gated. It refuses
unless every file in the worktree's diff is marked reviewed **and** the worktree is
clean (all work committed — you cannot merge uncommitted or untracked changes). It
then merges the worktree's branch into the base branch (in whichever worktree has
base checked out) and removes the worktree. If the merge hits a conflict it is
aborted, leaving the base branch untouched — a failed approve never corrupts main.

Review marks survive commits: only files whose content actually changed flip back to
unreviewed, so committing work no longer resets the review state of other files.

## Tests

Test-first throughout; 39 tests, green under `-race`:

```sh
go test ./...
go test -race -count=1 ./...
```

Two behaviours were caught by tests and fixed during development: untracked files were
missing from diffs (agents create new files — now synthesized read-only via
`git diff --no-index`), and content edits to already-dirty files weren't detected (the
"changed?" decision is now keyed on the diff hash, correct under polling).

## Guardrails

Declarative rules (`internal/guardrail`), shipped defaults catch: edits under
`migrations/`, edits to `.github/workflows/*`, large single-file net deletions, and
worktree-wide "deletes far more than it adds". Rules are data, so growing the set is a
config change, not code.

## Not yet built (deliberately, next phases)

Bubble Tea TUI (richer than the ANSI client), the web reading-room with side-by-side
diffs, an fsnotify watcher, and a SQLite store — all drop in behind the interfaces above
without reworking the engine. (These need external modules; this build is pure stdlib
because the module proxy was unavailable when it was written.)
