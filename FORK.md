# Fork notes

This is a fork of [microsoft/typescript-go](https://github.com/microsoft/typescript-go).
It exists to expose a wrapper API on top of the TypeScript 7 native
checker so that [jetlint](https://github.com/jetlint/jetlint) (a fast,
type-aware TypeScript linter) can consume types, symbols, signatures,
and nodes from outside the `internal/` tree.

Upstream's `pkg/checker` is intentionally narrow. The wrapper commits
on this fork add accessors and helpers — they don't change behavior,
they only widen the public API.

## Branch layout

- **`main`** — tracks `microsoft/typescript-go` main. No fork-specific
  commits live here. Updated by fetching upstream and fast-forwarding.
- **`tsgolint-wrapper`** — `main` plus the wrapper-API commit stack.
  This is the branch jetlint depends on. Released versions are tagged
  on this branch (`v0.1.0`, `v0.2.0`, ...).

## How jetlint consumes the fork

jetlint's `go.mod` has:

```go
require github.com/microsoft/typescript-go v0.1.0
replace github.com/microsoft/typescript-go => github.com/jetlint/typescript-go v0.1.0
```

The module path stays `microsoft/typescript-go` so upstream type names
keep resolving; the `replace` directive rewrites resolution to fetch
from this fork's tagged release.

## Refresh workflow: rebasing onto upstream

One-time setup:

```bash
git remote add upstream https://github.com/microsoft/typescript-go.git
```

Each refresh cycle:

```bash
# 1. Pull the latest upstream
git fetch upstream

# 2. Replay the wrapper stack on top of new upstream tip
git rebase upstream/main tsgolint-wrapper
# (or in jj: jj rebase -d upstream/main -s <first-wrapper-commit>)

# 3. Resolve conflicts as they appear. The wrapper API is additive,
#    so most rebases conflict only when upstream renames or refactors
#    something the wrappers expose. Fix the wrapper internals;
#    keep the exported surface in pkg/checker stable.

# 4. Run jetlint against the rebased fork (sibling-checkout layout)
#    to confirm wrappers still compile and tests pass:
#      cd ../jetlint && go test -short ./...
#    Temporarily switch jetlint's go.mod replace line back to
#      replace github.com/microsoft/typescript-go => ../typescript-go
#    for this verification, then revert before committing.

# 5. Tag and push
NEW=v0.2.0   # bump appropriately
git tag $NEW -m "$NEW: rebase onto upstream <date>"
git push origin tsgolint-wrapper --force-with-lease
git push origin $NEW

# 6. In jetlint, bump go.mod
#    require github.com/microsoft/typescript-go vX.Y.Z
#    replace ... => github.com/jetlint/typescript-go vX.Y.Z
#    go mod tidy
#    git commit -m "build: bump typescript-go fork to vX.Y.Z"
```

Force-push on `tsgolint-wrapper` is intentional and safe — the branch
has no external contributors and rebase is the whole point.

## Versioning

Use semver pre-1.0 (`v0.x.y`):

- **Minor bump (`v0.X.0`)** — rebase onto a new upstream tip, or add
  wrapper API surface. Either may break jetlint at the type level, so
  every refresh bumps minor for safety.
- **Patch bump (`v0.x.Y`)** — fix a wrapper bug without changing the
  exposed surface or the upstream base.

A `v1.0.0` is reserved for the point at which either (a) the wrapper
commits are upstreamed and the fork is retired, or (b) the fork's
wrapper surface is declared stable.

## Exit strategy

The long-term goal is to upstream the wrapper commits to
`microsoft/typescript-go` and retire this fork. Each rebase is a
natural checkpoint to review which wrapper commits applied cleanly
and look idiomatic — those are upstream-PR candidates. As they land
upstream, drop them from the wrapper stack here, shrinking the fork
toward zero.

## Why wrapper commits stay small and self-describing

Each commit on `tsgolint-wrapper` adds one accessor or one small
group of related accessors with a `feat(checker): expose X` subject.
This shape is deliberate: it keeps rebase conflicts localized
(usually a single commit touches a single upstream file), and it
makes individual commits straightforward to lift into upstream PRs
later. Resist the temptation to bundle wrapper additions into larger
"batch" commits.
