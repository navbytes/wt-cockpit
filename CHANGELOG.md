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

### Fixed
- Unicode and space-containing filenames in diffs are now decoded correctly (core.quotePath handling).
- Approve gate now re-diffs the worktree under lock before merging, preventing a commit landing between review and approve from bypassing the gated write path.

### Changed
- Review state schema is new (`reviewed_files` field); v0.1 review state is dropped silently on first load (migration is automatic, no user action needed).
- `[[rules]]` section in the config file replaces the built-in defaults entirely (append-only would make defaults non-disableable). Omit the section to keep built-in rules.
- Committing work no longer resets review state on other files in the same worktree.
- Bare `wt` (no arguments) now opens the full-screen TUI when stdout is a terminal — the new daily-driver default. Piped or redirected output (`wt | grep`, `wt > out.txt`, cron, CI) is unaffected and still prints the existing `ls` text radar. `wt ls` and `wt tui` remain explicit, TTY-independent entry points.

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
