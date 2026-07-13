# Changelog

## Unreleased

### Added
- Per-file review identity: review marks now survive commits. Only files whose content actually changed flip back to unreviewed; each file's hash includes git blob IDs for real content identity (including binary files).
- fsnotify git-state-first watcher: watches `.git/HEAD`, `.git/index`, and refs for changes (with fallback to polling for network mounts). Enabled by default; use `-watch fsnotify|poll` to select.
- Config file `~/.config/wtcockpit/config.toml` (or `$XDG_CONFIG_HOME/wtcockpit/config.toml`): define roots, per-repo base branch overrides, guardrail rules, and daemon options. Precedence: explicit flags > config file > built-in defaults. Config file present-but-malformed is a fatal error (typos in guardrails won't silently fail).
- `-config` flag to specify an alternate config file path.
- Full-screen terminal UI (`wt tui`): a live Radar view (sidebar + virtualized, chroma-syntax-highlighted unified diff, guardrail banners) and a Review view (per-file checklist, progress bar, `a` to approve & merge). Adds sidebar search (`/`), active-only filter (`f`), force refresh (`R`), and jumping to the tmux pane `cd`'d into a worktree (`t`). Built on Bubble Tea v1; colors degrade truecolor → 256 → 16 automatically and `NO_COLOR` renders plain text.
- `internal/client`: one shared HTTP/SSE client for both the CLI and the TUI (previously an inline ~70-line copy in `cmd/wt`), removing a source of CLI/TUI drift.
- `GET /api/diff` now reports each file's reviewed state (`Diff.Reviewed`), so the TUI's diff pane can show a per-file ✓ without a second round trip.
- **Comments**, the review→agent feedback loop: inline notes anchored to a file (and optionally a specific line — line 0 is a file-level comment), persisted alongside review state. `wt comments <id> [--json] [--all]` (default: open only), `wt comment <id> <file> <line> [--old] [--author <name>] <body…>`, `wt resolve <id> <comment-id>`. REST surface (`GET`/`POST /api/comments`, `POST /api/comments/resolve`, `POST /api/comments/delete`) is available on every listener — the socket is tokenless for agents/the CLI, same as every other route. Each comment carries `stale`/`orphaned` booleans computed fresh on every read against the file's *current* content hash — never auto-resolved or auto-deleted by drift, so an agent (or a human) always decides for itself whether an anchor still applies. A new `comment.changed` SSE event fires on every add/resolve/delete. See **[docs/agents.md](docs/agents.md)** for the full agent-integration guide (the frozen `wt comments --json` schema, stale/orphaned semantics, exit codes).
- **The web reading room**: `wtd -web <host:port>` (or `web = "…"` in config.toml) opts a second, loopback-only listener into serving a server-rendered browser UI over the same engine/API the Unix socket already exposes — a deliberate, bigger-screen alternative to the TUI for side-by-side review. A live index of worktrees (`GET /`) and a per-worktree reading room (`GET /wt/<id>`): chroma-highlighted side-by-side hunks (striped filler cells on the shorter side of an unbalanced add/del block — a corrected version of the original UX mock's pairing), guardrail banners, per-file review checkboxes with a progress bar, the same gated approve button as the TUI, and inline comment threads (click a line's gutter, or a file's "comment" button for file-level notes; resolve/delete inline; amber "stale"/grey "orphaned" badges). Everything live-updates over SSE — worktree/diff/review/comment changes, including from a second tab, the CLI, or an agent — without a page reload. `wt open <id>` launches a browser straight to it ($BROWSER first, then the platform opener, then prints the URL as a last resort); `wt status`/`GET /api/status` report `webAddr` (empty when `-web` is off). No Node toolchain and no new dependencies: `html/template` + `embed` + one static vanilla-JS file for POSTs/SSE/fragment swaps.
- Browser-security middleware on the web listener, since it renders hostile, agent-authored bytes (diff content, filenames, comment bodies) to a browser on a port every website in it can address blindly: a Host allowlist (the load-bearing DNS-rebinding defense), Origin + `Sec-Fetch-Site` checks, a `crypto/rand` per-process CSRF token required on every state-changing request (constant-time compared, no cookies), and a locked-down CSP (`script-src 'self'`, no inline scripts/styles/handlers, `frame-ancestors 'none'`) plus `nosniff`/`X-Frame-Options`/`no-referrer`/`no-store` on every response. `-web` refuses any non-loopback bind outright (remote access is a later phase). Loopback is machine-wide, not private, the same trade-off `-tcp` already made — documented, not treated as a solved problem.

### Fixed
- Unicode and space-containing filenames in diffs are now decoded correctly (core.quotePath handling).
- Approve gate now re-diffs the worktree under lock before merging, preventing a commit landing between review and approve from bypassing the gated write path.
- **Security:** raw control bytes (including terminal escape sequences) in diff content, hunk headers, and file paths are now neutralized to visible caret notation (e.g. ESC → `^[`) before rendering, instead of passing through byte-for-byte. A worktree's tracked files/names are untrusted input — a hostile agent worktree could previously retitle your terminal, move the cursor, or worse, just by being displayed in `wt diff` or the TUI's diff pane. Side effect: the per-file review hash (which covers content) changes for any file that contained a raw control byte, so review marks on such files (rare in real code) reset once.
- Diff parsing no longer mistakes a deleted or added file's own content for a `--- `/`+++ ` path header when that content itself starts with `-- `/`++ ` — byte-identical to the header prefix once git's per-line delete/add marker is prepended — which previously corrupted the file's path and undercounted its stats. Per-file hashes shift for any file that hit this (a correctness fix, not data loss; review marks on such files, if any, reset once).
- **Security:** `-tcp` now applies the same Host + Origin/`Sec-Fetch-Site` checks the web listener uses (no CSRF token — CLI/agent compatibility). Previously a browser on the same machine could already form-POST `/api/approve` or any other mutating route at a running `-tcp` listener with no preflight; this closes that pre-existing hole for existing `-tcp` users. A bare CLI/agent request (no `Origin` header at all) is completely unaffected.
- **Security:** a pack glob (`path_glob`/`path_globs`/`exclude_globs` in a checked-in `.wtcockpit.toml`) could wedge a refresh — and so every refresh plus `wt approve`, which share the same lock — on a pathological pattern like repeated `**a`/`*a` matched against a long non-matching path. The old recursive "try every split point" matcher ran superpolynomially on such input; it's replaced with a bounded dynamic-programming scan (`O(len(pattern)*len(path))`, never exponential). Glob semantics are unchanged.
- **Security:** `wt menubar` now sanitizes the repo directory name and branch name before emitting a SwiftBar/xbar plugin line. Git allows a branch name to contain `|`, which SwiftBar/xbar treats as its own `title|params` delimiter (params include `bash=`/`shell=`, run on click) — an agent-named branch like `feat|bash=/tmp/evil.sh` previously became a clickable run-on-click row for whoever installed the plugin. `|` is now replaced with `¦`; raw control bytes (a repo directory name can carry these on some filesystems) render as caret notation, matching every other renderer of untrusted worktree text.
- **Security:** `wt rules` and `wt ls` now sanitize a rule's name/message/conditions (and `wt ls`'s guardrail-hit summary) before printing to the terminal. This pack-authored text is semi-trusted (a checked-in `.wtcockpit.toml`, not the untrusted agent worktree itself), but a TOML backslash-u escape decoding to a raw OSC/ANSI byte previously reached the terminal unsanitized — the same treatment the TUI and web guardrail banners already applied to this exact input was missing from these two CLI renderers.
- The default `secrets-pattern` rule's `sk-` (OpenAI-shaped key) branch no longer matches ordinary kebab-case identifiers that merely contain the substring "sk-" (e.g. `risk-management-dashboard`, `disk-usage-monitoring-service`) — the character class dropped `-`/`_`, so a real key (`sk-` plus 20+ plain alphanumeric characters) still matches.
- A daemon restart whose very first `Refresh` fails outright (a discovery error, or the context canceling mid-scan) no longer opens the cold-start gate — the next successful scan no longer replays every worktree's standing guardrail hits as "new".
- A `.wtcockpit.toml` pack whose own `[[rules]]` list contains two entries with the same name now fails to load (falling back to the global rules, same as any other malformed pack) instead of silently keeping only the last entry.
- Guardrail `Compile` now rejects a negative threshold value (`min_files_changed`, `min_total_changed`, `min_changed_lines`, `min_net_deleted`, `min_delete_add_ratio`, `min_token_entropy`) instead of silently accepting it as a permanent no-op rule.

### Changed
- Guardrail `Compile`'s validation was already strict about a `[[rules]]` entry's `severity` being one of `""`/`warn`/`danger` — noted here since it was previously undocumented: a v0.2-era `config.toml` (or a `.wtcockpit.toml` pack) using some other severity spelling fails the whole config/pack load rather than silently accepting or defaulting it. Intended tightening, not a regression.
- Review state schema is new (`reviewed_files` field); v0.1 review state is dropped silently on first load (migration is automatic, no user action needed).
- `[[rules]]` section in the config file replaces the built-in defaults entirely (append-only would make defaults non-disableable). Omit the section to keep built-in rules.
- Committing work no longer resets review state on other files in the same worktree.
- Bare `wt` (no arguments) now opens the full-screen TUI when stdout is a terminal — the new daily-driver default. Piped or redirected output (`wt | grep`, `wt > out.txt`, cron, CI) is unaffected and still prints the existing `ls` text radar. `wt ls` and `wt tui` remain explicit, TTY-independent entry points.
- The comment schema shipped in v0.1 (`WorktreeID, File, Line, Body, State, At`, unused — no writer ever shipped) is extended in place (`ID`, `Side`, `FileHash` added) and moved to `internal/model` alongside the other wire types; no migration needed, since nothing had ever written one.

## v0.1.0 — 2026-07-12

Initial release: the engine MVP, built test-first (39 tests, race-clean, pure
standard library).

### Added
- `wtd` daemon: repo/worktree discovery across roots, per-worktree structured
  diffs vs merge-base (committed + uncommitted + **untracked** files), declarative
  guardrail engine with default rules (migrations, CI workflows, large deletions,
  net-negative churn), review store, delta event bus, HTTP + SSE API over a Unix
  socket with optional TCP.
- `wt` client: `ls` (radar), `watch` (live via SSE), `diff`, `review`,
  `approve`, `refresh`.
- Gated approve→merge write path: refuses unless every changed file is reviewed
  and the worktree is clean; merges into the base branch where it is checked out;
  aborts on conflict leaving base untouched; removes the worktree on success.
- Swappable seams: `gitbackend.Backend` (git CLI now; libgit2/gitoxide later),
  `watcher.Watcher` (poller now; fsnotify later), `store.Store` (JSON now;
  SQLite later).

### Known limitations
- Change detection is polling (2s default); fsnotify lands in v0.2.
- Review state resets when a worktree's diff hash changes (including on commit);
  per-file review identity lands in v0.2.
- Agent detection is heuristic (marker files) and often reports `unknown`.
