# Contributing

Thanks for taking a look. A few things keep this codebase easy to work on — please
keep them true.

## Ground rules

**Test-first.** Every behaviour change starts with a failing test. The suite runs
in seconds (`make test`) and must stay green under the race detector (`make race`);
CI enforces both plus `gofmt` and `go vet`.

**The engine/frontend split is load-bearing.** Frontends (the `wt` client, future
TUI/web/menu-bar) hold no git logic — they only speak the daemon's HTTP/SSE API.
Conversely, nothing in `internal/` may import a terminal or HTTP concern.

**Swappable seams stay seams.** Git access goes through `gitbackend.Backend`,
change detection through `watcher.Watcher`, persistence through `store.Store`. New
capabilities extend the interface; they don't bypass it.

**One write path.** `Engine.Approve` is the only operation that mutates a repo,
and its gates (full review + clean tree, abort-safe merge) are non-negotiable.
Everything else is read-only by design — that's the product.

**Guardrails are data.** New rule *kinds* extend `guardrail.Rule` with a new
condition field; new *rules* are config, not code.

## Dev loop

```sh
make lint test    # fast feedback
make race         # before pushing
make build && ./bin/wtd -root ~/code &
./bin/wt watch
```

The `internal/*_test.go` files build real temporary git repos — look at
`engine_test.go`'s `buildWorkspace` for the pattern. No mocks of git; we test
against the real thing.

## Commit style

Conventional-ish: `feat(engine): …`, `fix(diffparse): …`, `docs: …`. Keep the
subject under ~70 chars; explain the *why* in the body when it isn't obvious.
