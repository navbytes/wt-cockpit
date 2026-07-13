# Agent integration guide

wt cockpit is read-only and agent-agnostic: it never launches or drives an
agent. What it *does* give an agent is a feedback channel — a human (or
another agent) reviews a worktree's diff in the [reading room](../README.md)
or the TUI, leaves inline comments, and your agent picks those comments up and
acts on them. This is the "review → agent" loop v0.4 closes.

The loop, end to end:

1. A reviewer opens `wt open <id>` (or the TUI) and leaves comments against
   specific lines (or a whole file — see "Anchoring" below).
2. Your agent polls `wt comments <id> --json` (or watches `/api/events` for
   `comment.changed` — see "Watching instead of polling") between turns.
3. For each comment with `"state": "open"`, the agent reads `file`/`line`,
   makes the change, and calls `wt resolve <id> <comment-id>` once it's
   addressed the feedback.
4. The reviewer sees the resolution (the resolved badge, or a shrinking open
   count) and either re-reviews or approves.

Everything here is over the same Unix socket API the CLI and TUI use — no
special agent mode, no separate token. If your agent can run a shell command,
it can use this.

## Polling: `wt comments`

```sh
wt comments <id>              # open comments only (the default), human-readable
wt comments <id> --json       # same filter, machine-readable
wt comments <id> --all --json # every comment, open and resolved
```

`<id>` is the worktree id from `wt ls`/`wt status`, or (for a script that
already knows which worktree it's running in) can be derived once and cached
— it's stable for the worktree's lifetime.

The `--json` output is `CommentsPayload`, decoded straight off the daemon's
`GET /api/comments` response and re-emitted with `encoding/json`'s
`MarshalIndent` — the CLI never reformats or renames a field, so this is
field-for-field (identical fields/order/values; CLI pretty-prints) what the
API returns. That is a deliberate design invariant
(P4-design.md §1.5): **the API and the CLI can never drift apart**, so you
can point an agent at either one.

### The schema (frozen)

```json
{
  "worktreeId": "api-server-auth-refactor",
  "path": "/Users/me/code/api-server-wt/auth-refactor",
  "branch": "auth-refactor",
  "base": "main",
  "comments": [
    {
      "id": "c-1a2b3c4d5e6f7081",
      "worktreeId": "api-server-auth-refactor",
      "file": "internal/auth/token.go",
      "line": 19,
      "side": "new",
      "body": "widen to 30m only for refresh tokens, not access tokens",
      "author": "naveen",
      "state": "open",
      "fileHash": "9f2c…",
      "at": "2026-07-13T10:11:12Z",
      "stale": false,
      "orphaned": false
    }
  ]
}
```

| Field | Meaning |
|---|---|
| `worktreeId`, `path`, `branch`, `base` | Where the worktree actually lives on disk and what it's diffed against — enough for an agent to `cd $path`, check out nothing else, and open `file:line` directly, all from this one JSON blob. |
| `comments[].id` | Stable identifier (`"c-" + 16 hex chars`). Pass this straight to `wt resolve`. |
| `file` | Worktree-relative path, exactly `DiffFile.Path` — the same string `wt diff`/the reading room use. |
| `line` | The commented line number. **`0` means a file-level comment** (not anchored to any specific line — general feedback about the file as a whole). |
| `side` | Which numbering `line` uses: `"new"` (the working-tree version, the default) or `"old"` (the base version — relevant on a comment about a deleted or since-moved line). |
| `body` | The reviewer's note, plain text. Never markdown, never HTML — render/print it verbatim. |
| `author` | Free text; defaults to the reviewing machine's OS username if the reviewer didn't set one. Don't treat it as an authenticated identity — this is a single-user tool. |
| `state` | `"open"` or `"resolved"`. Act on `"open"` ones; `wt resolve` is what flips a comment to `"resolved"`. |
| `fileHash` | The file's content hash *at the time the comment was written* — the comment's anchor baseline. |
| `at` | RFC 3339 timestamp. |
| `stale` | See below. |
| `orphaned` | See below. |

Stability promise: this is a wire contract, governed by the same rule as the
daemon↔client protocol (see [ROADMAP.md](../ROADMAP.md)) — **a minor version
may add fields, only a major version may remove or rename one.** Code that
decodes this JSON into a struct with unknown fields ignored (as
`encoding/json` does by default) never breaks across a minor upgrade.

## Anchoring: `stale` and `orphaned`

Comments are anchored to a file's content hash, not to a line number alone —
the file keeps changing under an agent's feet, and a plain line number goes
stale the instant a line is inserted above it. Two independent booleans tell
you exactly how much to trust the anchor, computed **fresh on every read**
(never cached, never persisted) against the worktree's current diff:

- **`stale: true`** — the file is still part of the diff, but its content has
  changed since the comment was written (`fileHash` no longer matches the
  file's current hash). The line number *might* still be right, or the file
  may have shifted enough that it no longer is. **Re-read the file around
  that line before trusting it** — don't blindly patch line 19 just because
  the comment says line 19.
- **`orphaned: true`** — the file isn't in the diff at all any more (deleted,
  renamed away, or edited back to be identical to `base`). There is nothing
  to anchor to; treat the comment as informational only, or as a signal the
  reviewer's feedback may already be moot.

Both flags exist because a comment is **never auto-resolved or auto-deleted**
by drift — only a human or agent decides, via `wt resolve`/the reading room's
delete button, when a comment stops mattering. A comment that never goes
stale or orphaned (the common case — you address the feedback promptly) needs
no special handling at all: read it, fix it, resolve it.

## Resolving: `wt resolve`

```sh
wt resolve <id> <comment-id>
```

This is the agent's loop-closer — call it once you've made the change the
comment asked for. There is no `wt comment --delete`: deleting a comment
outright (as opposed to marking it resolved) is a reviewing-human action, done
from the reading room's UI — an agent's role in this loop is to resolve, not
to delete someone else's note. If a comment is genuinely moot (its file is
gone), `orphaned: true` already tells you that; resolving it is still the
right move so it stops showing up in the default `open`-only list.

There is no "reopen" — if a resolved comment turns out to need more work,
leave a fresh one (`wt comment`).

## Watching instead of polling

If polling on a timer isn't a good fit for your agent's loop, subscribe to
the daemon's SSE stream instead of re-running `wt comments` on an interval:

```sh
curl -N --unix-socket "$WTD_SOCKET" http://localhost/api/events
```

Every comment add/resolve/delete publishes one `comment.changed` event
(`{"type":"comment.changed","id":"<worktreeId>","at":"..."}`) — no payload
(the bus only ever ships deltas, never full state), so treat it purely as a
signal to re-run `wt comments <id> --json`. If `-web` is enabled, the same
stream is reachable at `http://<webAddr>/api/events` (behind the browser
security middleware; the socket has none of that and needs no token).

## Exit codes

Every `wt` subcommand, `comments`/`comment`/`resolve` included, follows the
same simple contract: **exit `0` on success, exit `1` on any failure**, with a
human-readable message on stderr prefixed `wt: `. There are no
per-failure-reason exit codes to switch on — script against stderr text (or
just check the exit code and, on failure, decide whether to retry) rather
than assuming a specific number means a specific thing. Common failure
reasons you'll hit from an agent: the worktree id doesn't exist any more (it
was approved and removed — see `orphaned`/watch for `worktree.removed`), the
comment id was already resolved or never existed, or `wtd` isn't reachable at
all.

## Adding comments (for completeness — usually a human's job)

Agents are consumers of this loop far more often than producers of it, but
nothing stops an agent leaving its own comments (e.g. flagging something for
a human to double-check):

```sh
wt comment <id> <file> <line> [--old] [--author <name>] <body…>
wt comment my-worktree internal/auth/token.go 19 "double check this expiry math"
wt comment my-worktree internal/auth/token.go 0 "this whole file could use a second pass"   # line 0 = file-level
```

`--author` defaults to the OS user running `wt` if omitted — set it
explicitly (e.g. `--author claude-code`) so a human reviewer can tell an
agent-authored comment apart from their own.
