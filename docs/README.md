# core-vcs docs

The design of record for this library is **core-agent's**
[`docs/gitops-vcs-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/gitops-vcs-design.md).
Its "Settled decisions" section binds this repository: decisions 1–3 and
6–9 describe the library, 10–11 the split and versioning, 14–15 the
checkout and forge-less-remote rules. That doc borrows from kube-agents'
`docs/designs/version-control-support.md` (§2 concepts, §3 the guard
arguments, §5 modularity, §7 GitLab) its vocabulary, publish guards and
error taxonomy — not its wire protocol.

Nothing is duplicated here. When a decision about the library's own
surface needs recording — a provider-interface change, an error-code
addition, a new guard — it gets a `docs/<topic>-design.md` in this repo
with a "Settled decisions" section and an "Out of scope" list, registered
below, and core-agent's doc gains a pointer.

## Local designs

- [`api-design.md`](./api-design.md) — the exported surface of package
  `vcs` (core-vcs#2): `RepoRef`, `Forge`, `Repo`, `Credential`,
  `Provider`, `Error`/codes, `Registry`/`Catalog`, and where the
  `Workspace` guard state lives.
