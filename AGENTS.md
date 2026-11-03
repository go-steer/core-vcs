# core-vcs project memory

When an AGENTS.md-aware agent runs inside this repo, this file is loaded
into its system prompt as the project-level instruction prefix. Keep it
short and load-bearing.

## What this project is

`core-vcs` is a forge-neutral version-control library in pure Go for agents
that run where there is no `git` binary — a distroless container being the
case it was built for. It gives an agent a go-git working copy and a set of
neutral verbs (`clone`, `commit`, `publish`, `proposal-create`, …) that a
provider package translates to one forge's API: GitHub (github.com and
GitHub Enterprise Server), GitLab (gitlab.com and self-managed), a bare git
remote, and later others.

It was split out of [`go-steer/core-agent`](https://github.com/go-steer/core-agent)
the same way `core-models` and `core-tui` were: a pluggable-backend layer
behind a narrow interface, released on the backends' cadence, importable
without an agent runtime. core-agent's `pkg/tools/vcs` is the first
consumer; kube-agents' Go side, k8s-lookout, cogo and scion are the next.

**Source of truth.** The design of record is core-agent's
[`docs/gitops-vcs-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/gitops-vcs-design.md),
which in turn adopts kube-agents'
`docs/designs/version-control-support.md` for vocabulary, verbs, error
codes and guards. Read both before changing anything exported.
[`docs/README.md`](./docs/README.md) says what is local to this repo.

## The rules that make this library what it is

- **Neutral vocabulary, everywhere exported.** A change offered for review
  is a `proposal`. `pull request` and `merge request` appear only inside a
  provider package and in a provider's `proposalNoun` string. A tool name,
  a field, a type or an error code that says PR or MR is a bug.
- **The agent never writes the target branch.** `Workspace.Publish`
  refuses `TARGET_IS_BRANCH` and `BRANCH_NOT_OURS` before any network. No
  option relaxes them. There is no force-push verb.
- **Only the remote half is abstracted.** `status`, `diff`, `log`,
  `commit` are go-git on the local copy and go through no provider.
  Anything that crosses the network or spends a credential is a provider
  verb.
- **One directory per provider, one registry line.** A new forge is
  `providers/<name>/` plus one line in the registry. The parameterised
  contract test must pass before it registers; the boundary test fails
  any file outside `providers/<name>/` that names that forge's hosts or API
  paths.
- **Thin clients over `net/http`.** No forge SDKs. go-git is the one
  heavy dependency; keep the rest of the graph small, because
  core-agent's `verify-slim-deps` watches what we pull in.
- **Credentials are values behind the `Credential` interface.** A
  provider receives a token at call time and never reads an env var or a
  file itself. Per-caller identity plugs in behind the same seam.
- **Errors carry a code from the shared set** (`BASE_MOVED`,
  `PROTECTED_BRANCH`, `FORGE_CALL_FAILED`, …) and the forge's own message
  in `Detail`. A caller sees the code, never a bare HTTP status.

## Layout

```
vcs/                 the library: RepoRef, Forge, Credential, Provider,
                     Workspace (go-git), registry, error codes.
providers/
  git/               bare remote; publish only, optional proposal link.
  github/            github.com + GHES.
  gitlab/            gitlab.com + self-managed (private CA).
internal/            test fixtures (httptest forges, local remotes), helpers.
examples/            runnable examples; each is run by CI or listed as excluded.
dev/                 build, test, lint, CI tooling; see dev/README.md.
docs/                what is local to this repo; the design lives in core-agent.
```

## Build & test

```bash
dev/tools/ci             # full local CI, fast-fail order
dev/tools/build          # go build ./...
dev/tools/test-unit      # go test -race -coverprofile, all packages
dev/tools/lint-go        # golangci-lint (pinned version auto-installs)
dev/tools/fix-go-format  # gofmt + goimports
```

The default test run needs no network and no tokens. Tests against a real
forge are gated on an env var and skip cleanly without it.

## Conventions

- **License headers everywhere.** Every Go, shell, YAML and Python source
  file starts with the full Apache 2.0 boilerplate attributed to Google
  LLC. `goheader` enforces it for `.go`; run `dev/tools/add-license-headers`
  for the rest.
- **Commits.** Small, self-contained, Conventional Commits subjects. The
  body says *why* and what verification was done. Sign off with DCO
  (`git commit -s`).
- **No agent attribution of any kind.** No `Co-Authored-By` naming an
  agent, no "Generated with" footer, no agent as author or committer, in
  commits, PR titles or PR bodies.
- **Tests before merging.** Every new package ships with unit tests. A bug
  fix needs a regression test that fails on the pre-fix code.
- **A provider ships with its contract-test fixture.** Recorded
  `httptest` responses for every verb and every error code, including the
  self-hosted host form.
- **Run `dev/tools/ci` before every push.** It runs the same scripts CI
  runs.
- **Adversarial review before every PR touching Go code.** A skeptic
  review of the staged diff, findings fixed or pinned, outcome recorded in
  the PR body under `## Adversarial review`.
- **Versioning.** `v0.x` until the second provider ships; then apidiff
  and an API freeze at `v1.0.0`. core-agent pins a tagged version and
  bumps it as a separate PR.

## How we develop

One long-lived branch, `main`. Short-lived branches (`feat/…`, `fix/…`,
`chore/…`, `docs/…`), each with a PR. Rebase, don't merge. A PR merges when
the required checks are green.

## Pitfalls

- **go-git shallow clones break ancestry.** `Depth` makes `publish`'s
  `BASE_MOVED` check lie and `log` incomplete. Single-branch is the size
  control; there is no shallow option.
- **GHES is `/api/v3`, GitLab self-managed is `/api/v4`, both under the
  forge's host.** github.com alone is `api.github.com`. Keep the base-URL
  rule inside the provider.
- **GitLab project paths have arbitrary depth** (`group/sub/project`) and
  must be URL-encoded as one path segment in API calls.
- **A test that proves a guard must fail when the guard is commented out.**
  Verify it before opening the PR.
