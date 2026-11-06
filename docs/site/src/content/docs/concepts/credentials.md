---
title: Credentials & TLS
description: Supply tokens via CredentialFunc (outbound proxy), environment variables, files, or static values, and configure custom CAs.
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

### `TokenFromFile` & `TokenFromEnv` (local & Kubernetes Secret mounts)

- `vcs.TokenFromFile(path)` reads and trims `path` on each call, so rotated Kubernetes Secret volume projections are picked up without restarting the process.
- `vcs.TokenFromEnv(name)` reads `$name` on each call.
- `vcs.StaticToken(token)` holds a fixed token in memory (useful in tests).

## Secret redaction

All built-in `Credential` implementations and `*vcs.BasicAuth` implement `fmt.Stringer` and `fmt.GoStringer` returning `"[REDACTED]"`. Printing a `Forge`, `Repo`, or `BasicAuth` with `%v`, `%+v`, `%#v`, or `%s` never leaks a token into logs.

## Self-hosted TLS (`Forge.CAFile`)

For self-managed GitLab, GitHub Enterprise Server, or bare Git remotes behind a private certificate authority, set `Forge.CAFile` to the path of a PEM bundle:

```go
forge := vcs.Forge{
	Name:     "corp-gitlab",
	Provider: "gitlab",
	Host:     "gitlab.internal.example",
	CAFile:   "/etc/ssl/certs/corp-root-ca.pem",
	Cred:     vcs.TokenFromFile("/var/run/secrets/gitlab/token"),
}
```

Both `vcs.NewHTTPClient(forge)` (used by provider REST clients) and `vcs.Clone` / `Workspace.Publish` (used by `go-git` smart-HTTP transport) trust `Forge.CAFile` in addition to the system root pool, and classify untrusted certificate errors as `CodeForgeTLSUntrusted` (`FORGE_TLS_UNTRUSTED`) naming `CAFile`.
