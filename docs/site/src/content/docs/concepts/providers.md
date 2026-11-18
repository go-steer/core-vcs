---
title: Providers
description: Built-in providers (github, gitlab, git) and how to implement and test a new Provider.
sidebar:
  order: 5
---

`providers.NewRegistry()` registers all built-in providers:

| Provider name | Package | Hosts & API | Proposal noun |
|---|---|---|---|
| `"github"` | `providers/github` | `github.com` (`https://api.github.com`) and GitHub Enterprise Server (`https://<host>/api/v3`, or custom `Forge.APIBaseURL`) | `"pull request"` |
| `"gitlab"` | `providers/gitlab` | `gitlab.com` (`https://gitlab.com/api/v4`) and self-managed GitLab (`https://<host>/api/v4`, arbitrary-depth `group/sub/project` paths) | `"merge request"` |
| `"git"` | `providers/git` | Any smart-HTTP Git server (`https://<host>/<path>.git`) | `"link"` (when `Forge.ProposalURLTemplate` is set) or `"missing"` |

## Provider specifics

### GitHub (`providers/github`)

- **Clone auth**: HTTP Basic with username `"x-access-token"` and the resolved token as password.
- **Proposals**: Maps `Proposal` to `/repos/{owner}/{repo}/pulls` and `/repos/{owner}/{repo}/issues/{number}` (for labels and general discussion comments), merging general issue comments, review summaries, and inline review comments into chronological order on `ProposalComments`.
- **Reviews & merges**: Implements `ProposalReviewer` (`POST /repos/{owner}/{repo}/pulls/{number}/reviews`, checking `head.sha` before and after submission when `ExpectedRevision` is set so concurrent pushes return `PROPOSAL_HEAD_MOVED`, and requiring a non-empty `Body` on `request_changes` and `comment` reviews) and `ProposalMerger` (`PUT /repos/{owner}/{repo}/pulls/{number}/merge` with atomic `sha: ExpectedRevision`, mapping `409` head drift to `PROPOSAL_HEAD_MOVED`, `405` base-branch-modified races to `FORGE_UNAVAILABLE`, and `405`/`422` protection/approval/check/`mergeable_state=blocked` blocks to `PROTECTED_BRANCH`, followed by best-effort `DELETE /repos/{owner}/{repo}/git/refs/heads/{source}` only when `head.repo` is non-null, matches the target repository, and `source` is neither the base branch nor protected).
- **Bot detection**: Strips `"[bot]"` from `Actor.Login` and sets `Actor.Bot = true` when GitHub reports `type: "Bot"`.

### GitLab (`providers/gitlab`)

- **Nested groups**: Supports arbitrary namespace depth (`group/sub/project`), URL-encoding the full path as a single segment (`/api/v4/projects/group%2Fsub%2Fproject/...`).
- **Draft proposals**: Uses GitLab's `"Draft: "` title prefix convention on create and update, stripping the prefix on read so `Proposal.Title` and `Proposal.Draft` behave identically to GitHub.
- **Reviews & merges**: Implements `ProposalReviewer` (verifying `sha` and `diff_refs.head_sha`, posting inline comments via `/discussions`, summary notes via `/notes`, recording approvals via `/approve` with `GET /approvals` disambiguation on `401` for idempotent retries vs `user_can_approve=false` `FORGE_REJECTED`, and revoking prior approvals via `/unapprove` on `request_changes`) and `ProposalMerger` (`PUT /projects/{id}/merge_requests/{iid}/merge` with atomic `sha: ExpectedRevision`, refusing `MergeRebase` with `FORGE_UNSUPPORTED`, mapping `409` SHA drift to `PROPOSAL_HEAD_MOVED`, and inspecting `detailed_merge_status` via `GET /projects/{id}/merge_requests/{iid}` on generic `405`/`406`/`422` responses to classify policy/approval/CI/discussion blocks as `PROTECTED_BRANCH` vs draft/conflict/rebase refusals as `FORGE_REJECTED`).
- **Protected branches**: Treats `push_access_levels` where every level is `0` (`No one`) or `> 30` (Maintainer-only) as protected unless `allow_force_push` is enabled.

### Bare Git remote (`providers/git`)

- Supports `Clone` and `Publish` against any HTTPS smart-HTTP Git remote.
- When `Forge.ProposalURLTemplate` is set (for example, `"https://git.corp.example/{path}/+/compare/{base}...{branch}"`), `Capabilities.ProposalNoun` is `"link"` and `CreateProposal` returns a `Proposal` with `URL` populated from the template (`{host}`, `{path}`, `{base}`, `{branch}` URL-escaped).
- When `ProposalURLTemplate` is empty, `Capabilities.ProposalNoun` is `"missing"` and proposal verbs return `CodeForgeUnsupported` (`FORGE_UNSUPPORTED`) without touching the network.

## Writing a new provider

A new forge is one directory `providers/<name>/` implementing [`vcs.Provider`](/reference/api-surface/) (plus optional [`vcs.ProposalReviewer` and `vcs.ProposalMerger`](/reference/api-surface/) when the forge supports structured reviews and merges) and one line in `providers/providers.go`. Every provider must pass two automated gates before registering:

1. **Contract test (`internal/vcstest.Run`)**: Exercises host resolution, path-traversal rejection, capability reporting, every served verb, and every induced `vcs.Code` against an `httptest` fixture (including self-hosted + private-CA variants).
2. **Boundary sweep (`internal/vcstest.CheckBoundary`)**: Walks the Go AST of the entire module and fails if any `.go` file outside `providers/<name>/` references that forge's hostnames, API base paths, or auth headers.
