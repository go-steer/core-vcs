# core-vcs

A forge-neutral version-control library in pure Go, for agents that run
where there is no `git` binary.

`core-vcs` gives an agent a [go-git](https://github.com/go-git/go-git)
working copy and a small set of neutral verbs — `clone`, `commit`,
`publish`, `proposal-create`, `proposal-view`, … — that a provider package
translates to one forge's API:

| Provider | Covers |
|---|---|
| `github` | github.com, GitHub Enterprise Server |
| `gitlab` | gitlab.com, self-managed GitLab (private CA supported) |
| `git`    | any bare git remote over HTTPS; publish a branch, optionally emit a compare link |

A change offered for review is a **proposal**. Pull request and merge
request are the same thing on two forges, and nothing exported here uses
either spelling.

Two things are true of every verb:

- **The agent never writes the target branch.** `Publish` refuses a branch
  that is the base branch, or one this process did not create, before
  anything leaves the process. There is no force-push.
- **Only the remote half is abstracted.** Status, diff, log and commit are
  go-git on the local copy. Only what crosses the network or spends a
  credential goes through a provider.

## Status

Pre-release (`v0.x`). The API churns until the second provider ships.
The first consumer is
[`go-steer/core-agent`](https://github.com/go-steer/core-agent)'s `vcs_*`
tool suite, which lets its distroless image open a proposal instead of
pushing to production.

## Design

The design of record is core-agent's
[`docs/gitops-vcs-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/gitops-vcs-design.md).
It borrows the `proposal` vocabulary, the publish guards and the error
taxonomy of kube-agents' `version-control-support.md`; it does not adopt
that design's broker protocol, which follows from a sandbox topology this
library's consumers do not have. [`docs/README.md`](./docs/README.md) says what is
local to this repository.

## Developing

```bash
dev/tools/ci        # everything CI runs, in fast-fail order
```

See [`AGENTS.md`](./AGENTS.md) for conventions and
[`CONTRIBUTING.md`](./CONTRIBUTING.md) for the PR process.

## License

Apache 2.0. See [`LICENSE`](./LICENSE).
