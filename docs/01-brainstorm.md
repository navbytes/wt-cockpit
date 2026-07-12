# Agent Diff Cockpit — Brainstorm

*A tool for watching, catching mistakes in, and reviewing diffs across multiple projects and multiple worktrees while AI agents (Claude Code, Codex, Aider, plain git) work in parallel.*

---

## 1. The problem, stated sharply

You run several agents at once, each in its own worktree, spread across multiple projects, mostly inside tmux/CLI now. VS Code's diff view worked when you had one project open, but it falls apart when the truth is: **N projects × M worktrees, all changing at once.** You lost the "at a glance, what did the robot just touch, and is it wrong?" view.

Two jobs you actually named:

- **Catch mistakes fast** — spot an agent going off the rails *while* it's writing, not after it's committed a mess.
- **Review & approve** — read the diff deliberately before you fold a worktree back into main.

These are different modes. The first wants a *radar* (glanceable, live, low-friction, sorted by "what changed most / most recently"). The second wants a *reading room* (side-by-side, syntax-highlighted, per-file "reviewed" toggles, maybe inline notes). A good tool serves both without making you switch apps.

---

## 2. What already exists (so you don't rebuild it)

The space is crowded, but it splits into three groups — and the group you actually want is the thinnest.

### Group A — Orchestrators (they *own* the agents)

These launch and manage the agents for you, and diff-viewing is a feature bolted on top.

- **Conductor** — native macOS app; sidebar of sessions, worktree isolation, checkpoint snapshots, diff + merge review. Closest polished GUI to what you're imagining, but it wants to *be* your agent launcher.
- **parallel-code** (johannesjo) — Electron desktop app: "Run Claude Code, Codex, and Gemini side by side, each in its own worktree." Built-in diff viewer with inline comments, per-commit navigation, changed-files panel, per-branch merge. Very close to your feature list — but again, it spawns the agents.
- **Vibe Kanban / amux / Mux / Sculptor / Superset / Nimbalyst** — web or desktop, kanban-style, worktree-isolated, diffs shown on a board. Heavier, team/PM-flavored.
- **Claude Squad / dmux / Paneflow** — TUI/terminal, tmux-based, worktree-per-agent. Terminal-native, but thin on actual side-by-side diff review, and they too are launchers.

### Group B — Worktree managers (no diff)

- **Git-Worktree-Visualizer** (PeterHdd) — tiny Python TUI: list worktrees, dirty/clean status, ahead/behind, open one in a tmux split. Explicitly **no diff viewing**. This is the skeleton of your navigation layer with the actual payload missing.

### Group C — Pure diff-review TUIs (single repo)

- **Deff** — Rust TUI, side-by-side diffs, syntax highlighting, per-file reviewed toggles, `--include-uncommitted`. Excellent *reading room*, but single-repo and single-diff; not multi-worktree/multi-project aware, and no live watching.
- **difi**, **diff-tui**, **delta** — similar single-repo diff renderers.
- **tuicr / meatcheck** — add inline annotation so an agent can consume your review comments and iterate. Interesting adjacent idea.

### The gap

Almost everything good is an **orchestrator that insists on launching the agents itself**. You already launch your own agents your way (tmux, four different CLIs, hand-made worktrees). What's missing is a **read-only, agent-agnostic diff cockpit** that:

- attaches to worktrees that *already exist*, no matter who created them,
- aggregates **across projects**, not just within one repo,
- shows **live** diffs (updates as files change on disk),
- and lets you flip into a deliberate side-by-side review + approve/merge when you're ready.

Deff + Git-Worktree-Visualizer, fused and made multi-project and live, is basically the product. Nobody in the list does exactly that without also wanting to own your agents.

**Conclusion: yes, it makes sense to build — but as a passive *viewer/reviewer*, deliberately NOT an orchestrator.** That constraint is your whole edge: it works with Claude Code, Codex, Aider, and bare git identically, because it only reads the filesystem and git.

---

## 3. Form factor — TUI vs macOS app vs menu-bar (you asked me to decide)

Scoring against your reality (already in CLI/tmux, multiple projects, "catch fast" + "review deliberately", solo builder):

| Dimension | TUI | Native macOS (Swift) | Menu-bar + local web view |
|---|---|---|---|
| Fits current tmux/CLI flow | **Best** — lives where you are | Weak — a separate window to alt-tab to | Medium |
| Works over SSH / remote box | **Yes** | No | Only if you tunnel |
| Side-by-side reading quality | OK (limited by terminal) | **Best** | **Best** (real HTML diff) |
| Glanceable "radar" + notifications | Medium (needs a pane) | **Good** (native notifs) | **Good** (menu-bar badge + notifs) |
| Build effort (solo) | **Low** | High (Swift, app lifecycle, signing) | Medium |
| Cross-platform later | **Yes** | No | Yes |

### Recommendation: **TUI-first core, with an optional local web "reading room."**

Concretely, a phased bet:

1. **Phase 1 — TUI radar (the thing you'll use daily).** A terminal cockpit: left pane = tree of *projects → worktrees* with live status (files changed, +/- lines, "agent active?" heuristic, dirty/clean, ahead/behind). Right pane = the diff of the selected worktree, live-updating. Keyboard-driven, tmux-friendly, SSH-friendly. This alone solves "catch mistakes fast," and it's the cheapest to build.

2. **Phase 2 — Reading room.** For deliberate review, press a key to open the current worktree's diff in a richer side-by-side view. Cheapest high-quality path: spin up a tiny local web server and open the browser (real HTML side-by-side beats anything a terminal can render), with per-file "reviewed" toggles and an **approve → merge worktree to main** action. This reuses your engine and avoids Swift entirely.

3. **Phase 3 (optional) — menu-bar presence.** A small macOS menu-bar app that shows a badge ("3 worktrees with unreviewed changes") and fires a native notification when an agent's diff crosses a threshold (e.g. touched >20 files, or edited a path you marked sensitive like `migrations/` or `.github/`). It just talks to the same local server.

**Skip the full native Swift app.** It's the most work for the least marginal gain here: the menu-bar + web combo already gives you native notifications and beautiful diffs, while staying cross-platform and SSH-capable. Only go full-native if this becomes a product you sell to Mac users who never touch a terminal — not your use case.

---

## 4. MVP — what to build first

**Core principle: it only *reads* git and the filesystem. It never launches or controls agents.** That's what makes it work with all four of your setups at once.

**MVP scope (Phase 1 TUI):**

- **Discovery** — point it at a set of root folders (or auto-scan for `.git`). Enumerate every repo and every `git worktree list` entry across all of them. One flat, sortable view of *all* worktrees everywhere.
- **Live status per worktree** — poll `git status --porcelain` + `git diff --numstat` (or watch the filesystem via fsnotify/watchman) to show: files changed, +/- line counts, last-modified time, dirty/clean, ahead/behind upstream. Sort by "most recently changed" so an actively-writing agent floats to the top.
- **"Agent active" heuristic** — you don't need agent APIs. Infer activity from: files changed in the last N seconds, or an optional convention (agent writes a `.agent-status` file / you tag the tmux window). Show a pulse indicator.
- **Diff pane** — selected worktree's `git diff` (working tree vs its base branch), syntax-highlighted, live-refreshing. Reuse `delta`-style rendering or a library rather than writing a differ.
- **Jump to session** — `t` to focus/open the matching tmux window or worktree shell (borrow the Git-Worktree-Visualizer trick).

**Fast-follow (Phase 2):**

- **Reading room** — `r` opens side-by-side HTML diff in browser; per-file reviewed checkboxes; in-diff search.
- **Approve & merge** — from a reviewed worktree, one action to merge/rebase back to main and optionally remove the worktree.
- **Guardrail alerts** — flag diffs that touch sensitive globs, delete >X lines, or add secrets-shaped strings, so "off the rails" surfaces itself.

**Explicit non-goals (at least at first):** spawning agents, managing prompts, cloud/team boards, PR integration. Every one of those is a competitor's whole product; staying a pure viewer is the moat.

**Stack suggestion:** Go or Rust for a single static binary (no runtime to install, easy to drop on any box, SSH-friendly). Go + Bubble Tea, or Rust + Ratatui, are the obvious TUI choices; both have mature git and fsnotify bindings, and the Phase-2 web server is trivial in either.

---

## 5. Open questions worth deciding before you write code

- **Watch vs. poll.** Filesystem watching (watchman/fsnotify) is instant but fiddly across many repos; polling every 1–2s is dead simple and probably fine. Start polling, upgrade later if it feels laggy.
- **Diff baseline.** Working tree vs the worktree's branch point? Vs `main`? Vs last commit? "Everything this worktree changed since it forked from main" is usually what you mean when catching an agent — make that the default.
- **How much to lean on `delta`.** Shelling out to an existing renderer gets you beautiful diffs on day one; embedding your own gives control. Start by shelling out.

---

### One-line verdict

Build it — as a **read-only, agent-agnostic diff cockpit, TUI-first with a local web reading room**, deliberately *not* an orchestrator. That niche (multi-project × multi-worktree, live, watches agents it didn't launch) is the one gap the 2026 tools left open.
