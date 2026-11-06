---
title: API surface
description: Exported Go packages, types, and functions in core-vcs (v1.x compatibility guarantee).
sidebar:
  order: 1
---

The exported API surface of `core-vcs` is frozen at `v1.0.0` and guarded in CI by `dev/tools/verify-apidiff` against [`dev/api-surface.txt`](https://github.com/go-steer/core-vcs/blob/main/dev/api-surface.txt).

## Packages

| Package | Stability | Purpose |
|---|---|---|
| `github.com/go-steer/core-vcs/vcs` | `promised` | Core types: `RepoRef`, `Forge`, `Catalog`, `Repo`, `Credential`, `Workspace`, `Policy`, `Provider`, `Error`, `Code` |
| `github.com/go-steer/core-vcs/providers` | `promised` | `NewRegistry()` with `github`, `gitlab`, and `git` pre-registered |
| `github.com/go-steer/core-vcs/providers/github` | `promised` | `github.com` and GitHub Enterprise Server provider |
| `github.com/go-steer/core-vcs/providers/gitlab` | `promised` | `gitlab.com` and self-managed GitLab provider |
| `github.com/go-steer/core-vcs/providers/git` | `promised` | Bare smart-HTTP Git remote provider |

## `vcs` core types

### Repository & catalog

```go
type RepoRef struct {
	Host string // lowercase, port preserved when non-standard
	Path string // slash-separated, no leading/trailing slash, no ".git"
}

func ParseRepoRef(value string, knownHosts ...string) (RepoRef, error)

type Role string

const (
	RoleGitOps  Role = "gitops"  // primary read-write repository (at most one per Catalog)
	RoleManaged Role = "managed" // additional read-write repository
	RoleContext Role = "context" // read-only reference repository
)

type Forge struct {
	Name                string
	Provider            string     // "github" | "gitlab" | "git"
	Host                string     // empty = provider default
	Scheme              string     // "" or "https"
	Port                int        // 0 = scheme default
	CAFile              string     // PEM bundle for private CA
	Username            string     // clone basic-auth user for "git" provider
	ProposalURLTemplate string     // "git" provider compare link template ({host} {path} {base} {branch})
	CommitIdentity      Identity
	Cred                Credential // nil for anonymous read-only
}

type RepoConfig struct {
	Forge      string
	Repository string
	Role       Role
	BaseBranch string
}

func NewCatalog(reg *Registry, forges []Forge, repos []RepoConfig) (*Catalog, error)
func (c *Catalog) Resolve(value string) (Repo, error)
func (c *Catalog) GitOps() (Repo, bool)
func (c *Catalog) NewRepo(cfg RepoConfig) (Repo, error)
func (c *Catalog) Repos() []Repo
func (c *Catalog) Forges() []Forge
```

### Credentials (`vcs.Credential`)

```go
type Credential interface {
	Token(ctx context.Context) (string, error)
}

type CredentialFunc func(ctx context.Context) (string, error)

func StaticToken(token string) Credential
func TokenFromEnv(name string) Credential
func TokenFromFile(path string) Credential
```

### `Workspace` & `Policy`

```go
const (
	DefaultMaxCloneBytes int64 = 256 << 20
	DefaultBranchPrefix        = "agent/"
)

type Policy struct {
	MaxCloneBytes int64
	BranchPrefix  string
	CacheDir      string // shared bare object cache across sessions and processes
}

func Clone(ctx context.Context, repo Repo, branch, dir string, policy Policy) (*Workspace, error)

func (w *Workspace) Repo() Repo
func (w *Workspace) Dir() string
func (w *Workspace) ClonedBranch() string
func (w *Workspace) ClonedRevision() string
func (w *Workspace) CurrentBranch() (string, error)
func (w *Workspace) HEAD() (string, error)
func (w *Workspace) Status() (Status, error)
func (w *Workspace) Diff(opts DiffOptions) (string, error)
func (w *Workspace) Log(limit int) ([]Commit, error)
func (w *Workspace) Branch(name string) error
func (w *Workspace) Commit(msg string, identity Identity) (Commit, error)
func (w *Workspace) Publish(ctx context.Context, branch string) (PublishResult, error)
```

### Proposals (`Repo` methods)

```go
func (r Repo) Capabilities(ctx context.Context) (Capabilities, error)
func (r Repo) BaseBranchName(ctx context.Context) (string, error)
func (r Repo) ProtectedBranch(ctx context.Context, branch string) (bool, error)

func (r Repo) CreateProposal(ctx context.Context, req ProposalRequest) (Proposal, error)
func (r Repo) ListProposals(ctx context.Context, f ProposalFilter) (ProposalList, error)
func (r Repo) GetProposal(ctx context.Context, number int) (Proposal, error)
func (r Repo) ProposalComments(ctx context.Context, number int, p Page) (CommentList, error)
func (r Repo) ProposalCommits(ctx context.Context, number int, p Page) (CommitList, error)
func (r Repo) CommentProposal(ctx context.Context, number int, body string) (Comment, error)
func (r Repo) UpdateProposal(ctx context.Context, number int, u ProposalUpdate) (Proposal, error)
func (r Repo) CloseProposal(ctx context.Context, number int) (Proposal, error)
```
