# Changelog

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
