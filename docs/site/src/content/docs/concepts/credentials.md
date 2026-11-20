---
title: Credentials & TLS
description: Supply tokens via CredentialFunc (outbound proxy), mounted secret files (TokenFile), or StaticToken, and configure custom CAs.
sidebar:
  order: 4
---

A provider in `core-vcs` never reads environment variables or files on its own. Every credential is passed as a [`vcs.Credential`](/reference/api-surface/) value on `vcs.Forge` and resolved at call time via `Token(ctx context.Context) (string, error)`.

```go
type Credential interface {
	Token(ctx context.Context) (string, error)
}
```

## Credential constructors

### `CredentialFunc` (production outbound proxy / dynamic minting)

In production deployments (such as `core-agent` with an outbound credential proxy like `steward` or a GitHub App installation token minter), adapt any function with [`vcs.CredentialFunc`](/reference/api-surface/):

```go
forge := vcs.Forge{
	Name:     "github",
	Provider: "github",
	Cred: vcs.CredentialFunc(func(ctx context.Context) (string, error) {
		return credProvider.Resolve(ctx, "github-app", "acme-gitops")
	}),
}
```

Because `Token(ctx)` is called on each network operation (`Clone`, `Publish`, `CreateProposal`, …), short-lived tokens are refreshed automatically and per-caller `context.Context` values flow through to your resolver.

### `TokenFile` & `StaticToken` (Kubernetes Secret mounts & local tokens)

- `vcs.TokenFile(path)` reads and trims `path` on each call, so rotated Kubernetes Secret volume projections are picked up without restarting the process.
- `vcs.StaticToken(token)` holds a fixed token in memory (for tests, or when a CLI consumer reads an environment variable at startup via `vcs.StaticToken(os.Getenv("GITHUB_TOKEN"))`).

## Secret redaction

Every built-in `Credential` implementation and `vcs.BasicAuth` overrides `String`, `GoString`, and `Format` so tokens never leak when a `Forge`, `Repo`, or `BasicAuth` is formatted with `%v`, `%+v`, `%#v`, or `%s`:

- `CredentialFunc` and `StaticToken` print `"[redacted]"`.
- `TokenFile(path)` prints `"TokenFile(<path>)"` (the path, never the file's contents).
- `BasicAuth` prints `"BasicAuth(<username>)"` (the username, withholding `Password`).

## Self-hosted TLS (`Forge.CAFile`)

For self-managed GitLab, GitHub Enterprise Server, or bare Git remotes behind a private certificate authority, set `Forge.CAFile` to the path of a PEM bundle:

```go
forge := vcs.Forge{
	Name:     "corp-gitlab",
	Provider: "gitlab",
	Host:     "gitlab.internal.example",
	CAFile:   "/etc/ssl/certs/corp-root-ca.pem",
	Cred:     vcs.TokenFile("/var/run/secrets/gitlab/token"),
}
```

Both `vcs.NewHTTPClient(forge)` (used by provider REST clients) and `vcs.Clone` / `Workspace.Publish` (used by `go-git` smart-HTTP transport) trust `Forge.CAFile` in addition to the system root pool, and classify untrusted certificate errors as `CodeForgeTLSUntrusted` (`FORGE_TLS_UNTRUSTED`) naming `CAFile`.
