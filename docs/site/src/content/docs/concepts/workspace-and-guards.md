---
title: Workspace & publish guards
description: How Workspace protects the base branch and enforces in-memory guards before any network push.
sidebar:
  order: 2
---

A [`Workspace`](/reference/api-surface/) is a single-branch, non-shallow `go-git` working copy created by [`vcs.Clone`](/reference/api-surface/).

## Why guards live in Go memory

In an agent container, the working tree at `dir` — including `.git/config` and `.git/refs/` — is writable by the model's file tools. If `core-vcs` trusted `.git/config` to tell it which remote URL to push to or which branches the agent created, a prompt injection or errant file edit could rewrite `.git/config` and trick `Publish` into pushing to `main`.

Instead:

1. **No `Open(dir)` constructor**: The only way to obtain a `*Workspace` is `vcs.Clone`. If the process restarts, any leftover directory has no `*Workspace` in memory and must be cloned fresh.
2. **In-memory anchor state**: `*Workspace` records `clonedBranch`, `clonedRevision` (the exact commit hash checked out by `Clone`), and `ours` (the map of branches created via `Workspace.Branch` in this process, plus each branch's `lastPublished` commit hash).
3. **In-memory remote**: `Clone` strips the `origin` remote from `.git/config`, and `Publish` constructs its `git.Remote` in memory directly from `Repo.CloneURL()`.

## The seven `Publish` guards

When `ws.Publish(ctx, branch)` is called, every guard runs in strict order:

| # | Guard | Error code | When it fires |
|---|---|---|---|
| 1 | Role check | `ROLE_VCS_VERBS` | Repository has `RoleContext` (checked before network). |
| 2 | Target branch check | `TARGET_IS_BRANCH` | `branch` equals `ClonedBranch()` or `Repo.BaseBranch` (checked before network). |
| 3 | Branch ownership & prefix | `BRANCH_NOT_OURS` | `branch` does not start with `Policy.BranchPrefix` (default `"agent/"`), was not created by `ws.Branch` in this process, or its local tip does not descend from `ClonedRevision()` (checked before network). |
| 4 | Base branch history check | `BASE_MOVED` | On first publish, the remote target branch was deleted or rewritten so `ClonedRevision()` is no longer an ancestor of the remote target tip. (An ordinary fast-forward advance on the target branch passes.) |
| 5 | Remote branch collision / drift | `BRANCH_DIVERGED` / `BRANCH_MOVED` | On first publish, `branch` already exists on the remote (`BRANCH_DIVERGED`). On subsequent publishes, the remote tip of `branch` no longer matches our `lastPublished` hash (`BRANCH_MOVED`), or our local tip is not a fast-forward of `lastPublished` (`FORGE_REJECTED`). |
| 6 | Forge branch protection | `PROTECTED_BRANCH` | `Repo.ProtectedBranch(ctx, branch)` reports that the forge protects `branch`. |
| 7 | Non-forced push | `FORGE_REJECTED` | Pushes with `Force: false` and CAS `RequireRemoteRefs` on subsequent publishes. |

## Working-tree size ceiling (`MaxCloneBytes`)

`Policy.MaxCloneBytes` (default `256 MiB`) caps the total size of checked-out files in the working tree (excluding `.git/`). If a repository exceeds the ceiling, `Clone` removes `dir` and returns `CodeCloneTooLarge` (`CLONE_TOO_LARGE`).

> **Note:** `core-vcs` never uses shallow clones (`Depth`). In `go-git`, shallow clones truncate commit ancestry, which breaks `Log` and causes `Publish`'s `BASE_MOVED` ancestry check to lie. Single-branch checkout + `Policy.CacheDir` is the size control.
