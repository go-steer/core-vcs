---
title: Architecture & catalog
description: How Registry, Forge, Catalog, RepoRef, Role, and Provider fit together.
sidebar:
  order: 1
---

`core-vcs` separates **configuration & routing** (`Registry`, `Forge`, `Catalog`, `Repo`), **local Git working trees** (`Workspace`), and **forge API backends** (`Provider`).

```
NewCatalog(reg, forges, repos)
      │
      ▼
   Catalog ──► Resolve("acme/infra") / GitOps()
                 ├── Local verbs via vcs.Clone(...) ──► Workspace (pure-Go go-git)
                 └── Remote verbs (CreateProposal, …) ──► Provider (github | gitlab | git)
```

## `RepoRef`: canonical repository identity

[`ParseRepoRef(value, knownHosts...)`](/reference/api-surface/) accepts any common repository spelling and normalises it to a `RepoRef{Host, Path}`:

| Input | `knownHosts` | Normalised `RepoRef.String()` |
|---|---|---|
| `"acme/infra"` | `[]string{"github.com"}` | `"acme/infra"` (resolved to `"github.com/acme/infra"` by `Catalog`) |
| `"https://github.com/acme/infra.git"` | `nil` | `"github.com/acme/infra"` |
| `"git@gitlab.example.com:platform/k8s/cluster.git"` | `nil` | `"gitlab.example.com/platform/k8s/cluster"` |

`ParseRepoRef` rejects empty segments, `.` or `..` path traversal, leading-dash segments (flag-injection defence), query strings, fragments, and embedded `user:pass@` URL credentials with `CodeRepoInvalid` (`REPO_INVALID`).

## `Catalog` & repository allowlisting

A [`Catalog`](/reference/api-surface/) is built once at startup via `NewCatalog(reg, forges, repos)`:

- **Every configured repository has an explicit `Role`**:
  - `RoleContext` (`"context"`): read-only reference repository. `Clone`, `Status`, `Diff`, `Log`, and read-only proposal verbs (`ListProposals`, `GetProposal`, `ProposalComments`, `ProposalCommits`) are permitted; any mutating verb (`Commit`, `Publish`, `CreateProposal`, `CommentProposal`, `UpdateProposal`, `CloseProposal`) is refused before network access with `CodeRoleVCSVerbs` (`ROLE_VCS_VERBS`).
  - `RoleGitOps` (`"gitops"`): the primary read-write repository (at most one per `Catalog`) where the agent commits, publishes a topic branch, and opens proposals. Retrieved via `Catalog.GitOps()`.
  - `RoleManaged` (`"managed"`): additional read-write repositories.
- **Unlisted repositories are refused (`REPO_UNKNOWN`)**: `Catalog.Resolve(value)` only resolves repositories declared in `repos`. If a Go caller deliberately wants to construct an ad-hoc `Repo` for a configured forge, it calls `Catalog.NewRepo(RepoConfig)` explicitly in Go code.
