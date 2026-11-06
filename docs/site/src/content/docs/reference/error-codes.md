---
title: Error codes
description: Every vcs.Code returned by core-vcs, when it fires, retryability, and how callers recover.
sidebar:
  order: 2
---

Every error returned by `core-vcs` unwraps to `*vcs.Error` and carries a machine-readable `vcs.Code`. Use `vcs.CodeOf(err)` or `vcs.IsCode(err, code)` to branch on errors, and `err.Code.Retryable()` to check whether a transient call can be retried.

```go
if vcs.IsCode(err, vcs.CodeBaseMoved) {
	// Target branch history was rewritten since Clone; clone again.
}
if vcs.CodeOf(err).Retryable() {
	// Only FORGE_UNAVAILABLE (HTTP 5xx or rate-limited 429/403) is retryable.
}
```

## Identity & configuration codes

| Code | Constant | Retryable | Meaning & recovery action |
|---|---|---|---|
| `CONFIG_INVALID` | `CodeConfigInvalid` | No | `Forge`, `CatalogConfig`, or `Policy` failed validation, or commit author identity is missing. Fix configuration. |
| `REPO_INVALID` | `CodeRepoInvalid` | No | `ParseRepoRef` or provider `Resolve` rejected the repository path (traversal, leading `-`, credentials in URL, wrong segment depth). |
| `REPO_UNKNOWN` | `CodeRepoUnknown` | No | Repository is not listed in `CatalogConfig.Repos` (or `Catalog.Repo("")` is ambiguous). Add the repository to the catalog. |
| `FORGE_UNSUPPORTED` | `CodeForgeUnsupported` | No | No configured forge serves this host, provider name, or verb (for example, `proposal-list` on a bare `git` remote). |
| `ROLE_VCS_VERBS` | `CodeRoleVCSVerbs` | No | A mutating verb (`commit`, `publish`, `proposal-create`, …) was called on a `RoleContext` repository. |

## Workspace & publish guard codes

| Code | Constant | Retryable | Meaning & recovery action |
|---|---|---|---|
| `TARGET_IS_BRANCH` | `CodeTargetIsBranch` | No | `Branch` or `Publish` was called with the base/target branch name. Create and publish a topic branch under `Policy.BranchPrefix`. |
| `BRANCH_NOT_OURS` | `CodeBranchNotOurs` | No | Branch does not start with `Policy.BranchPrefix`, was not created by `Workspace.Branch` in this process, or does not descend from `ClonedRevision()`. Clone again and create a branch via `Workspace.Branch`. |
| `BASE_MOVED` | `CodeBaseMoved` | No | Remote target branch was deleted or force-rewritten since `Clone` so `ClonedRevision()` is no longer an ancestor. Clone again and replay changes. |
| `BRANCH_DIVERGED` | `CodeBranchDiverged` | No | On first `Publish`, the topic branch already exists on the remote with history not published by this `Workspace`. Pick a new branch name. |
| `BRANCH_MOVED` | `CodeBranchMoved` | No | On a subsequent `Publish`, the remote tip of the topic branch no longer equals our `lastPublished` hash. Someone else pushed to the branch; clone again. |
| `PROTECTED_BRANCH` | `CodeProtectedBranch` | No | The forge marks the branch protected against push. Publish to an unprotected topic branch. |
| `BASE_BRANCH_MISSING` | `CodeBaseBranchMissing` | No | The configured or default base branch does not exist on the remote repository. |
| `CLONE_TOO_LARGE` | `CodeCloneTooLarge` | No | Checked-out working tree exceeds `Policy.MaxCloneBytes`; the directory was removed. |
| `NOTHING_TO_COMMIT` | `CodeNothingToCommit` | No | `Workspace.Commit` was called on a clean working tree or an empty index. |
| `GIT_FAILED` | `CodeGitFailed` | No | Underlying `go-git` or filesystem failure that does not map to a specific guard code. |

## Forge & network codes

| Code | Constant | Retryable | Meaning & recovery action |
|---|---|---|---|
| `FORGE_UNAUTHORIZED` | `CodeForgeUnauthorized` | No | HTTP 401, unthrottled HTTP 403, or empty/failed credential resolution. Stop and report the credential failure. |
| `FORGE_UNAVAILABLE` | `CodeForgeUnavailable` | **Yes** | HTTP 5xx, HTTP 429, rate-limited HTTP 403 (`Retry-After` / `X-RateLimit-Remaining: 0`), or network timeout/connection reset. Retry with backoff. |
| `FORGE_REJECTED` | `CodeForgeRejected` | No | HTTP 400 / 404 / 409 / 422 or non-fast-forward push rejection. Fix the request parameters or commit message. |
| `FORGE_CALL_FAILED` | `CodeForgeCallFailed` | No | Unexpected forge response or decode error; `Error.Detail` carries the forge's message verbatim. |
| `FORGE_TLS_UNTRUSTED` | `CodeForgeTLSUntrusted` | No | Server TLS certificate failed verification; set `Forge.CAFile` to the trusted PEM bundle. |
| `FORGE_IDENTITY_UNKNOWN` | `CodeForgeIdentityUnknown` | No | Credential cannot identify its forge login. |
| `LABELS_UNSUPPORTED` | `CodeLabelsUnsupported` | No | `ProposalRequest.Labels` or `ProposalUpdate.AddLabels`/`RemoveLabels` was used on a forge whose `Capabilities.Labels` is `false`. |
