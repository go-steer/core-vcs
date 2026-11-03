# Contributing to core-vcs

Thanks for your interest in contributing! This file is the table of contents — most of the detail lives in [`dev/README.md`](./dev/README.md) and [`AGENTS.md`](./AGENTS.md).

By participating in this project you agree to abide by the [Code of Conduct](./CODE_OF_CONDUCT.md).

## Reporting bugs and requesting features

- **Bugs:** [open an issue](https://github.com/go-steer/core-vcs/issues/new) with the forge (github.com, GHES `x.y`, gitlab.com, self-managed GitLab `x.y`, bare remote), the verb, the error code and `Detail` you got, your Go version, and the smallest reproduction. A recorded `httptest` fixture that reproduces it is the best bug report there is.
- **Feature requests:** check the design of record — core-agent's [`docs/gitops-vcs-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/gitops-vcs-design.md) — and the [open issues](https://github.com/go-steer/core-vcs/issues) first. File the use case before the proposed solution. A new forge is a feature request that names the forge's identity grammar, API base and credential shape.

## Pull requests

### Before you start

For anything beyond a typo fix or one-line bug, open an issue first so we can agree on the approach. Changes to anything exported from `vcs/` need to be discussed before implementation — core-agent pins this module and its `vcs_*` tools are written against it.

### Workflow

1. Create a short-lived branch off `main` (`feat/gitlab-provider`, `fix/base-moved-direction`, `docs/readme`).
2. Make your change. Keep the diff focused; unrelated cleanup belongs in a separate PR.
3. Run the full local CI before pushing:
   ```bash
   dev/tools/ci
   ```
   This is the same script that runs in GitHub Actions — green locally means green remotely.
4. Run an adversarial review over the staged diff for any change touching Go code, fix or pin every finding, and record the outcome in the PR body under `## Adversarial review`.
5. Open the PR against `main`. The required status checks (`test`, `lint`, `go mod tidy is clean`, `govulncheck`) gate the merge.

### Commit messages — Conventional Commits

Subject lines follow [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:` / `build:` / `ci:`. Optional scope in parens: `feat(gitlab): …`, `fix(workspace): …`. Keep the subject under ~70 chars; put *why* and what verification you did in the body.

### Developer Certificate of Origin (DCO)

All commits must be **signed off** under the [Developer Certificate of Origin](https://developercertificate.org/): pass `-s` to `git commit`. The name and email must match your `git config user.name` / `user.email`. If you forget, amend with `git commit --amend -s`.

**No agent attribution of any kind.** No `Co-Authored-By` naming an agent, no "Generated with" footer, no agent as author or committer. This applies to commits, PR titles and PR bodies.

### License headers

Every source file carries the full Apache 2.0 header attributed to Google LLC. `golangci-lint` enforces this on `.go` files via `goheader`. For new shell, YAML or Python files, run `dev/tools/add-license-headers` once — it is idempotent.

### Tests

- Unit tests live next to the code (`*_test.go`).
- Every provider passes the parameterised contract test against recorded `httptest` fixtures, including its self-hosted host form. A provider that does not pass it does not register.
- Workspace guards (`TARGET_IS_BRANCH`, `BRANCH_NOT_OURS`, `BASE_MOVED`, …) each have a test that fails when the guard is removed. Verify that before opening the PR.
- Tests against a real forge are gated on an environment variable and skip cleanly without it. The default run needs no network.
- A feature without a test is not done. A bug fix needs a regression test that fails on the pre-fix code.

## Project layout

- `vcs/` — the library: `RepoRef`, `Forge`, `Credential`, `Provider`, `Workspace`, registry, error codes.
- `providers/<name>/` — one directory per forge.
- `internal/` — fixtures and helpers.
- `dev/` — local + CI tooling.
- `docs/` — what is local to this repo; the design lives in core-agent.
- `.github/workflows/` — thin delegators to `dev/ci/presubmits/`.

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](./LICENSE).
