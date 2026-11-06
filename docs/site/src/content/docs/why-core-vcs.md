---
title: Why core-vcs
description: Why go-steer agents use a standalone, pure-Go, forge-neutral version-control library.
sidebar:
  order: 2
---

An autonomous SRE or GitOps agent diagnoses an incident by reading code and manifests, and proposes a fix by opening a change for human review. In production, that agent runs inside a locked-down distroless container with:

- **No `git` or `gh` / `glab` CLI binaries** in the image;
- **Multiple forges** across teams — `github.com`, GitHub Enterprise Server, `gitlab.com`, self-managed GitLab behind an internal CA, or a bare Git server;
- **Strict safety invariants** — the model has read/write access to its working directory (including `.git/`), yet must never push directly to `main` or overwrite a branch it did not create.

`core-vcs` was split out of [`go-steer/core-agent`](https://github.com/go-steer/core-agent) the same way [`core-models`](https://github.com/go-steer/core-models) and [`core-tui`](https://github.com/go-steer/core-tui) were: a pluggable-backend layer behind a narrow Go interface, importable without an agent runtime.

## Four design rules

### 1. Neutral vocabulary everywhere exported

GitHub calls a reviewed change a *pull request*; GitLab calls it a *merge request*; a bare Git server has no review entity at all or only a compare URL.

In `core-vcs`, a change offered for review is always a **`Proposal`**. `Capabilities.ProposalNoun` carries the forge's native term (`"pull request"`, `"merge request"`, `"link"`, or `"missing"`) so an agent's user-facing prompt or UI can use the local word, while every Go type (`Proposal`, `ProposalRequest`, `ProposalFilter`), verb (`proposal-create`, `proposal-view`, …), and error code stays 100% forge-neutral. An AST test (`vcs/vocabulary_test.go`) fails CI if any exported identifier outside `providers/` uses `PR` or `MR`.

### 2. Only the remote half is abstracted

Local operations (`Status`, `Diff`, `Log`, `Branch`, `Commit`) operate on the local [`Workspace`](/concepts/workspace-and-guards/) via `go-git` and never touch a [`Provider`](/concepts/providers/). Only operations that cross the network or spend a credential (`CloneURL`, `CloneAuth`, `BaseBranch`, `ProtectedBranch`, and the `Proposal` verbs) go through the `Provider` interface.

### 3. Guards live in Go memory, not in `.git/`

Because the working tree — including `.git/config` and `.git/refs/` — sits inside the agent's writable directory scope, nothing under `.git/` can be trusted to prove which branch is the base branch or whether this process created a branch:

- `Workspace` has no `Open(dir)` constructor — only [`Clone`](/reference/api-surface/).
- `clonedBranch`, `clonedRevision`, and the set of branches created via `Workspace.Branch` live in unexported fields on `*Workspace` in Go memory.
- `Workspace.Publish` constructs the remote in memory from `Repo.CloneURL()` (ignoring `.git/config`) and enforces `ROLE_VCS_VERBS`, `TARGET_IS_BRANCH`, `BRANCH_NOT_OURS`, `BASE_MOVED`, `BRANCH_DIVERGED` / `BRANCH_MOVED`, and `PROTECTED_BRANCH` before pushing.

### 4. Thin HTTP clients, small dependency graph

Each provider under `providers/<name>/` speaks REST over standard `net/http` using `vcs.NewHTTPClient(forge)`. `go-git` is the single heavy dependency; no GitHub or GitLab client SDKs are pulled in.
