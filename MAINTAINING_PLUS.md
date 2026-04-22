# Maintaining CLIProxyAPIPlus

This repository is maintained as a long-lived fork of upstream `router-for-me/CLIProxyAPI`.

The goal is:

- keep `main` close to upstream
- keep all third-party provider support on one long-lived integration branch
- make future upstream syncs incremental instead of repeating the original `archived-plus` merge

## Branch Model

Use exactly two long-lived branches:

- `main`
  - Tracks upstream as closely as possible.
  - Do not put third-party provider maintenance here.
  - Safe to fast-forward from `upstream/main`.
- `plus-main`
  - Long-lived integration branch for all community provider support.
  - This is the branch that contains `GitHub Copilot`, `Kiro`, `OpenCode`, `Cursor`, `CodeBuddy`, `GitLab Duo`, `iFlow`, and `Kilo`.
  - Merge upstream updates into this branch; do not repeatedly rebuild it from `archived-plus`.

Recommended rule: never develop provider features directly on `main`.

## Local Layout

Recommended local worktree layout:

- `/home/chesszyh/cliproxyapi/src`
  - checkout of `main`
- `/home/chesszyh/cliproxyapi/src-plus`
  - checkout of `plus-main`

This keeps upstream sync and fork maintenance physically separated.

If `src-plus` does not exist, recreate it from `src`:

```bash
cd /home/chesszyh/cliproxyapi/src
git fetch upstream origin --tags
git worktree add -b plus-main /home/chesszyh/cliproxyapi/src-plus origin/plus-main
```

If `plus-main` already exists locally, omit `-b plus-main`:

```bash
cd /home/chesszyh/cliproxyapi/src
git worktree add /home/chesszyh/cliproxyapi/src-plus plus-main
```

## One-Time History Notes

The original plus code was recovered from `../archived-plus`.

That recovery was distilled into two commits on top of upstream `v6.9.31`:

- `031c7519` `feat(providers): merge community provider support from archived-plus`
- `847eb767` `chore(fork): point maintenance tooling at Chesszyh fork`

Do not re-merge `archived-plus` again. Treat `plus-main` as the canonical maintained branch from now on.

## Required Git Config

Enable Git conflict memory. This matters for recurring upstream merges.

```bash
git config --global rerere.enabled true
git config --global rerere.autoupdate true
git config --global merge.conflictstyle zdiff3
```

`rerere` is especially useful here because the same integration files are likely to conflict repeatedly.

## Normal Update Workflow

### 1. Sync `main` with upstream

```bash
cd /home/chesszyh/cliproxyapi/src
git fetch upstream origin --tags
git switch main
git merge --ff-only upstream/main
git push origin main
```

`main` should stay clean. If `--ff-only` fails, stop and inspect why before proceeding.

### 2. Merge upstream into `plus-main`

```bash
cd /home/chesszyh/cliproxyapi/src-plus
git fetch upstream origin --tags
git switch plus-main
git merge upstream/main
```

Resolve conflicts, then validate:

```bash
GOTOOLCHAIN=auto go build -o /tmp/cli-proxy-api ./cmd/server
GOTOOLCHAIN=auto go test ./...
```

If validation passes:

```bash
git push origin plus-main
```

## Conflict Resolution Rules

When merging upstream into `plus-main`, use these rules.

### Keep upstream by default for:

- release packaging and artifact naming
- Docker image naming
- Goreleaser output naming
- most README branding changes
- upstream fixes that are unrelated to third-party providers

This fork intentionally keeps upstream packaging structure to reduce future drift.

### Re-apply plus logic on top of upstream for:

- `cmd/server/main.go`
- `internal/config/config.go`
- `internal/config/oauth_model_alias_defaults.go`
- `internal/registry/model_definitions.go`
- `internal/registry/model_registry.go`
- `internal/api/server.go`
- `internal/api/handlers/management/*`
- `internal/watcher/synthesizer/config.go`
- `sdk/cliproxy/service.go`
- `sdk/auth/*`
- `internal/runtime/executor/*`

These are the integration hotspots. Most real conflicts will happen here.

### Current policy decisions that should stay aligned with upstream

- Do not reintroduce obsolete Anthropic beta injection such as `context-1m-2025-08-07`.
- Keep upstream behavior when a mainline fix supersedes old plus behavior.
- Keep Amp proxy gzip handling compatible with upstream tests.

If a plus-era behavior conflicts with a newer upstream correctness fix, prefer upstream unless there is a verified provider regression.

## Commit Strategy

Keep commits small and layered.

Recommended split:

- provider/runtime changes
- config/model registry changes
- management/API surface changes
- fork-only maintenance changes

Avoid mixing these in one commit unless the change is inseparable.

Also keep fork-only changes separate from provider logic, for example:

- update checker URL pointing to `Chesszyh/CLIProxyAPIPlus`
- local debug scripts like `test_cursor.sh`
- temporary maintainer tooling under `cmd/`

This separation makes rebases, cherry-picks, and future cleanup much easier.

## Validation Checklist

Run this after any upstream merge or provider change:

```bash
cd /home/chesszyh/cliproxyapi/src-plus
GOTOOLCHAIN=auto go build -o /tmp/cli-proxy-api ./cmd/server
GOTOOLCHAIN=auto go test ./...
```

If only one provider area changed, still prefer full `go test ./...` before pushing.

At minimum, confirm these areas still pass:

- `internal/runtime/executor`
- `internal/api/modules/amp`
- `internal/watcher/synthesizer`
- `sdk/api/handlers/openai`
- `sdk/cliproxy/auth`

## Release Workflow

This fork's management update checker points to:

- `https://github.com/Chesszyh/CLIProxyAPIPlus`

So releases should be created in this fork if you want management-side version checks to work.

Recommended tag format:

- `v6.9.31-plus.1`
- `v6.9.31-plus.2`
- `v6.9.32-plus.1`

Where:

- `v6.9.32` is the upstream base version
- `plus.N` is the fork maintenance iteration on that upstream base

Example:

```bash
cd /home/chesszyh/cliproxyapi/src-plus
git switch plus-main
git tag v6.9.31-plus.1
git push origin v6.9.31-plus.1
```

## Adding or Modifying Providers

When extending provider support, follow this order:

1. auth
2. executor/runtime
3. config and model registry
4. watcher synthesis
5. SDK exposure
6. tests

Do not jump straight to translator-only changes without checking whether the provider also needs:

- login entry points in `cmd/server/main.go`
- config schema in `internal/config/config.go`
- auth synthesis in `internal/watcher/synthesizer/config.go`
- merged model exposure in `sdk/cliproxy/service.go`

## Upstreaming Guidance

If a fix is not inherently fork-specific, prefer sending it upstream.

Good upstream candidates:

- generic runtime bug fixes
- test fixes
- generic config synthesis fixes
- protocol handling fixes not tied to community-only providers

The smaller the permanent diff of `plus-main`, the cheaper long-term maintenance becomes.

## Recovery Notes

If a merge goes bad:

```bash
git merge --abort
```

If the worktree is already damaged but the branch is safe on remote:

```bash
cd /home/chesszyh/cliproxyapi/src
git worktree remove /home/chesszyh/cliproxyapi/src-plus
git worktree add /home/chesszyh/cliproxyapi/src-plus plus-main
```

If needed, rebuild from remote:

```bash
cd /home/chesszyh/cliproxyapi/src
git fetch origin
git branch -f plus-main origin/plus-main
```

## Practical Rule of Thumb

When in doubt:

- sync `main` first
- merge upstream into `plus-main`
- keep upstream fixes
- reattach provider support only where necessary
- run full tests
- push only after the branch is green
