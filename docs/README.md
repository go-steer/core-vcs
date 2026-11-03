# core-vcs docs

The design of record for this library is **core-agent's**
[`docs/gitops-vcs-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/gitops-vcs-design.md).
Its "Settled decisions" section binds this repository: decisions 1–3 and
6–9 describe the library, 10–11 the split and versioning, 14–15 the
checkout and forge-less-remote rules. That doc in turn adopts kube-agents'
`docs/designs/version-control-support.md` (§2 concepts, §3 seam and
protocol, §4 provider interface, §5 modularity, §7 GitLab) for the
vocabulary, verb set, error codes and publish guards.

Nothing is duplicated here. When a decision about the library's own
surface needs recording — a provider-interface change, an error-code
addition, a new guard — it gets a `docs/<topic>-design.md` in this repo
with a "Settled decisions" section and an "Out of scope" list, registered
below, and core-agent's doc gains a pointer.

## Local designs

(none yet)
