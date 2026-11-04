# Package `vcs`: the exported surface

> **Status — accepted, 2026-11-04.** Local design for
> [core-vcs#2](https://github.com/go-steer/core-vcs/issues/2). The design of
> record is core-agent's `docs/gitops-vcs-design.md`; this document only
> records the shape of the Go API that design leaves to this repository
> (decision 2: "the Go API is designed for Go consumers").

## What this decides

Issue #2 is the core: `RepoRef`, `Forge`, `Repo`, `Credential`, `Provider`,
`Error` and its codes, the registry, and the two tests that hold the
boundary. `Workspace` (#3) and the providers (#4, #5) are not designed here,
but every hook they need from the core is, and the section
[Workspace state](#workspace-state-what-3-will-build-on) says where the
guard state lives because that question shapes `Repo` and `Provider`.

## Settled decisions

1. **`Repo` is the API, `Provider` is the SPI.** A consumer resolves a
   `Repo` and calls verbs on it (`repo.CreateProposal(ctx, req)`). A
   provider package implements `Provider` and is never called directly by a
   consumer: `Repo` holds its provider in an unexported field. The role
   guard (`ROLE_VCS_VERBS`) and the capability guard (`FORGE_UNSUPPORTED`
   for a verb the forge reports `missing`) therefore live in one place,
   `Repo`, and no provider can forget them.

2. **`Provider.Resolve` canonicalises a `RepoRef`; the core builds `Repo`.**
   The design sketch has `Resolve(Forge, RepoRef) (Repo, error)`. `Role`
   and `BaseBranch` come from the repository entry, which a provider does
   not see, and attaching them is where the role guard is wired — so
   `Resolve` returns `(RepoRef, error)` (host already matched, grammar
   checked, path in the forge's canonical spelling) and `Catalog` assembles
   `Repo`. *Deviation from the design sketch; reason above.*

3. **Host matching is the core's, from a list the provider supplies.**
   `Provider.Hosts(Forge) []string` returns every spelling the forge answers
   to, canonical first (`github.com` and `www.github.com` for a `github`
   forge with no `Host`; the declared `Host` for GHES; `f.Host` alone for
   `git`). The core uses that list for three things: the schemeless
   `host/path` parse (the only place a host is inferred), the host check
   in `Catalog.Resolve`, and the refusal of a host two forges claim. No
   file outside `providers/github/` says `github.com`; the boundary test
   proves it.

4. **One parser, `ParseRepoRef`.** A host is read only where the syntax
   states one: `https://host/path`, `http://`, `ssh://[user@]host/path`,
   the SCP form `user@host:path`, or a schemeless value whose first segment
   is one of the hosts passed in. Everything else is a host-less path.
   `.git` and a trailing `/` are dropped; the host is lower-cased; the path
   keeps its case. Every segment is checked: non-empty, not `.` or `..`, no
   leading `-`, no whitespace, control, `\`, `:`, `?` or `#`. A port in the
   value stays on `Host` (`git.corp:8443`) and must match `Forge.Port`.
   Failure is `REPO_INVALID`. SCP and `ssh://` are accepted as *identity*
   syntax only; the clone URL is always the provider's HTTPS URL (design
   "Out of scope: SSH transport").

5. **`Catalog` is the resolution entry point; `Registry` is only
   name → constructor.** A `Catalog` is built once from a `Registry`, a
   `[]Forge` and a `[]RepoConfig`, and validation happens there: unknown
   provider, duplicate forge name, host claimed by two forges, repository
   naming an unknown forge, more than one `gitops` role, `BaseBranch` on a
   `context` entry, and whatever `Provider.ValidateForge` adds (`CAFile`
   on github.com, no `Host` on `git`). core-agent's `config.Validate`
   calls `NewCatalog` and reports its error; there is no second validator.

6. **No implicit fallback, anywhere.** A value whose host no forge serves
   is `FORGE_UNSUPPORTED`. A schemeless value with no host resolves only if
   it matches exactly one listed repository's path, or — when it matches
   none — exactly one forge is configured; two forges and no host is
   `FORGE_UNSUPPORTED` with "name the host" in `Detail`. A repository on a
   configured forge that is not in `repositories` is `REPO_UNKNOWN`: the
   token is the install's and the list is the allowlist of what it may be
   spent on. A consumer that keeps its own list calls `Catalog.NewRepo`
   with an explicit `RepoConfig` instead; that is the one way to reach an
   unlisted repository and it is spelled out.

7. **`Credential` is an interface; its sources are constructors.**
   `Token(ctx) (string, error)` is the W0 seam. `StaticToken(s)` and
   `TokenFile(path)` return unexported types whose `String`, `GoString`
   and `Format` print `[redacted]`, so a token cannot reach a log through
   `%v` of a `Forge`. `TokenFile` reads on every call, trimmed, because
   mounted secrets rotate. There is no `TokenEnv`: reading an env var is
   the consumer's (`config.token_env` → `StaticToken`), per AGENTS.md "a
   provider never reads an env var or a file itself". `Cred == nil` is
   anonymous and legal only where the provider says so (`git`, read-only).

8. **`CAFile` is read by the core, once, into a `*tls.Config`.**
   `vcs.NewHTTPClient(Forge)` and `vcs.TLSConfig(Forge)` build the
   transport every provider and the `Workspace` use; a provider that
   builds its own `http.Transport` fails the contract test's private-CA
   case. The no-file rule is about credentials; a CA bundle is public
   material, and resolving it in one place is what makes
   `FORGE_TLS_UNTRUSTED` say `CAFile` in every provider.

9. **Verbs are typed, neutral and kebab-cased.** `type Verb string` with
   `proposal-create`, `proposal-list`, `proposal-view`,
   `proposal-comments`, `proposal-comment`, `proposal-update`,
   `proposal-close`, `proposal-commits`, plus the local `clone`, `commit`,
   `publish`. `Capabilities.Verbs` / `Capabilities.Missing` are
   `[]Verb`; `Role.Allows(Verb)` is the role guard. No exported identifier
   or string value says PR or MR; a test enforces it over every package
   outside `providers/`.

10. **Reading a proposal is three verbs, not one with optional halves.**
    `GetProposal`, `ProposalComments` and `ProposalCommits` are separate;
    kube-agents' `proposal-view {comments?, diff?}` saved round trips over
    a wire and a Go caller composes calls instead. *Deviation from issue
    #2's verb list: adds `ProposalComments`; the review loop
    (`vcs_proposal_view` reading review comments) has no other home.*
    `Diff` is not a provider verb: the workspace has the objects.

11. **Error codes are a closed set in one file, each with a meaning and a
    next action.** `Error{Code, Forge, Verb, Detail}` wraps a cause.
    Consumers switch on `vcs.CodeOf(err)`; `Code.Retryable()` is true only
    for `FORGE_UNAVAILABLE`. The code set is issue #2's plus three the
    first slice needs: `REPO_INVALID` (parse), `REPO_UNKNOWN` (decision 6),
    `GIT_FAILED` (a go-git failure the workspace could not classify), and
    `NOTHING_TO_COMMIT` (reserved for #3). A provider maps every HTTP
    status to a code and puts the forge's own message in `Detail`; the
    contract test checks that no `Detail` is empty and no error escapes
    un-coded.

12. **Guards the core owns are answered from memory, never from disk.**
    See [Workspace state](#workspace-state-what-3-will-build-on). The core
    exposes no `Open(dir)`; a `Workspace` exists only as the return value
    of `Clone` in this process. That absence is the mechanism.

## Out of scope

- `Workspace` itself (#3), `providers/git` (#4), `providers/github` (#5).
- Issue verbs, labels as a first-class verb (`label-ensure`), `identity`,
  `branch-view` / `branch-delete`. The `Verb` constants are reserved so an
  addition is additive; `LABELS_UNSUPPORTED` is in the code set for the
  same reason.
- `proposal-acknowledge` — dropped (issue #2); it served kube-agents'
  review-comment ledger.
- Per-caller credentials, GitHub App token minting (P3). The seam is
  `Credential`.
- A `fileUrl` template in `Capabilities`. Useful for proposal bodies;
  additive when a recipe needs it.

## The surface

```go
package vcs // github.com/go-steer/core-vcs/vcs

// ---- identity -----------------------------------------------------------

// RepoRef is a repository's identity: a host where the syntax stated one,
// and an opaque, arbitrary-depth path. It carries no scheme and no port
// other than one the value spelled out.
type RepoRef struct {
	Host string // "" when the value named none; lower-case; may carry ":port"
	Path string // "owner/name", "group/sub/project"; no leading/trailing "/"
}

// ParseRepoRef is the one parser (decision 4). knownHosts drives the
// schemeless host/path form and nothing else.
func ParseRepoRef(value string, knownHosts ...string) (RepoRef, error)
func (r RepoRef) String() string     // "host/path" or "path"
func (r RepoRef) Segments() []string // Path split on "/"

// ---- configuration ------------------------------------------------------

type Identity struct{ Name, Email string }

// Forge is one entry in the consumer's forges list.
type Forge struct {
	Name                string     // unique within a Catalog
	Provider            string     // registry name: "github" | "gitlab" | "git"
	Host                string     // "" means the provider's public host; required for "git"
	Scheme              string     // "" or "https"; "http" only where the provider allows it
	Port                int        // 0 means the scheme default
	CAFile              string     // extra root CAs; resolved by TLSConfig
	Username            string     // clone basic-auth user for "git"; providers fix their own
	ProposalURLTemplate string     // "git" only: {host} {path} {base} {branch}
	CommitIdentity      Identity   // author/committer for Workspace.Commit
	Cred                Credential // nil is anonymous
}

type Role string

const (
	RoleGitOps  Role = "gitops"  // at most one per Catalog
	RoleManaged Role = "managed" // read-write
	RoleContext Role = "context" // read-only: mutating verbs refuse ROLE_VCS_VERBS
)

func (r Role) Allows(v Verb) bool

// RepoConfig is one entry in the consumer's repositories list.
type RepoConfig struct {
	Forge      string // Forge.Name
	Repository string // anything ParseRepoRef accepts
	Role       Role
	BaseBranch string // "" means ask the provider; refused on RoleContext
}

// ---- credentials --------------------------------------------------------

type Credential interface {
	Token(ctx context.Context) (string, error)
}

func StaticToken(token string) Credential // redacts under %v / %#v / %s
func TokenFile(path string) Credential    // re-read per call, trimmed

// ---- registry and catalog ----------------------------------------------

type Constructor func() (Provider, error)

type Registry struct{ /* unexported */ }

func NewRegistry() *Registry
func (r *Registry) Register(name string, c Constructor) error // duplicate is an error
func (r *Registry) MustRegister(name string, c Constructor)   // panics; for the providers table
func (r *Registry) Provider(name string) (Provider, error)    // FORGE_UNSUPPORTED
func (r *Registry) Names() []string

// Catalog is the validated set of forges and repositories (decision 5).
type Catalog struct{ /* unexported */ }

func NewCatalog(reg *Registry, forges []Forge, repos []RepoConfig) (*Catalog, error)
func (c *Catalog) Resolve(value string) (Repo, error) // decisions 4, 6
func (c *Catalog) Repos() []Repo                      // in config order
func (c *Catalog) GitOps() (Repo, bool)
func (c *Catalog) Forge(name string) (Forge, bool)
func (c *Catalog) NewRepo(cfg RepoConfig) (Repo, error) // explicit, unlisted

// ---- Repo: the consumer-facing handle ----------------------------------

type Repo struct {
	Forge      Forge
	Ref        RepoRef // canonical; Host always set
	Role       Role
	BaseBranch string // configured; "" until resolved by BaseBranchName

	provider Provider
}

func (r Repo) ProviderName() string
func (r Repo) CloneURL() string
func (r Repo) CloneAuth(ctx context.Context) (*BasicAuth, error)
func (r Repo) ProtectedBranch(ctx context.Context, branch string) (bool, error)
func (r Repo) Capabilities(ctx context.Context) (Capabilities, error)
func (r Repo) BaseBranchName(ctx context.Context) (string, error) // configured, else provider's

// Every mutating verb checks Role, then Capabilities, then calls the provider.
func (r Repo) CreateProposal(ctx context.Context, req ProposalRequest) (Proposal, error)
func (r Repo) ListProposals(ctx context.Context, f ProposalFilter) (ProposalList, error)
func (r Repo) GetProposal(ctx context.Context, number int) (Proposal, error)
func (r Repo) ProposalComments(ctx context.Context, number int, p Page) (CommentList, error)
func (r Repo) ProposalCommits(ctx context.Context, number int, p Page) (CommitList, error)
func (r Repo) CommentProposal(ctx context.Context, number int, body string) (Comment, error)
func (r Repo) UpdateProposal(ctx context.Context, number int, u ProposalUpdate) (Proposal, error)
func (r Repo) CloseProposal(ctx context.Context, number int) (Proposal, error)

// ---- Provider: the SPI one package per forge implements ----------------

type Provider interface {
	Name() string
	ValidateForge(f Forge) error                    // provider-specific config rules
	Hosts(f Forge) []string                         // canonical first; never empty
	Resolve(f Forge, ref RepoRef) (RepoRef, error)  // grammar + canonical path

	Capabilities(repo Repo) Capabilities // static half; Repo fills BaseBranch + Missing
	CloneURL(repo Repo) string                                   // HTTPS, no credential
	CloneAuth(ctx context.Context, repo Repo) (*BasicAuth, error)// nil = anonymous
	BaseBranch(ctx context.Context, repo Repo) (string, error)
	ProtectedBranch(ctx context.Context, repo Repo, branch string) (bool, error) // false when not knowable

	CreateProposal(ctx context.Context, repo Repo, req ProposalRequest) (Proposal, error)
	ListProposals(ctx context.Context, repo Repo, f ProposalFilter) (ProposalList, error)
	GetProposal(ctx context.Context, repo Repo, number int) (Proposal, error)
	ProposalComments(ctx context.Context, repo Repo, number int, p Page) (CommentList, error)
	ProposalCommits(ctx context.Context, repo Repo, number int, p Page) (CommitList, error)
	CommentProposal(ctx context.Context, repo Repo, number int, body string) (Comment, error)
	UpdateProposal(ctx context.Context, repo Repo, number int, u ProposalUpdate) (Proposal, error)
	CloseProposal(ctx context.Context, repo Repo, number int) (Proposal, error)
}

type BasicAuth struct{ Username, Password string } // Password redacts under %v

// Transport helpers every provider and the Workspace use (decision 8).
func TLSConfig(f Forge) (*tls.Config, error)
func NewHTTPClient(f Forge) (*http.Client, error)
func TransportError(f Forge, verb Verb, err error) *Error

// ---- vocabulary ---------------------------------------------------------

type Verb string

const (
	VerbClone, VerbCommit, VerbPublish                              Verb = "clone", "commit", "publish"
	VerbProposalCreate, VerbProposalList, VerbProposalView           Verb = "proposal-create", "proposal-list", "proposal-view"
	VerbProposalComments, VerbProposalComment, VerbProposalUpdate    Verb = "proposal-comments", "proposal-comment", "proposal-update"
	VerbProposalClose, VerbProposalCommits                           Verb = "proposal-close", "proposal-commits"
)

func (v Verb) Mutating() bool // commit, publish, proposal-create/-comment/-update/-close

// ProposalNoun values the core defines; a forge supplies its own otherwise.
const (
	ProposalNounMissing = "missing" // no proposal on this forge; the branch is the deliverable
	ProposalNounLink    = "link"    // git + ProposalURLTemplate: CreateProposal returns a URL
)

type Capabilities struct {
	ProposalNoun string // "pull request" | "merge request" | "link" | "missing"
	BaseBranch   string // effective base
	Verbs        []Verb // provider verbs this forge serves
	Missing      []Verb // provider verbs it does not
	Labels       bool   // proposals carry labels
}

// ---- proposals ----------------------------------------------------------

type ProposalState string

const (
	ProposalOpen   ProposalState = "open"
	ProposalClosed ProposalState = "closed"
	ProposalMerged ProposalState = "merged"
)

type Actor struct {
	Login string // forge login, "[bot]" and similar suffixes removed
	Bot   bool   // the forge's own answer, not the login's spelling
}

type Proposal struct {
	Number    int    // 0 when the forge has none (ProposalNounLink)
	URL       string
	Title     string
	Body      string
	State     ProposalState
	Source    string // branch
	Target    string // branch
	Revision  string // source tip as the forge last saw it
	Draft     bool
	Author    Actor
	Labels    []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type ProposalRequest struct {
	Source string   // required; the published branch
	Target string   // "" means the repository's base branch
	Title  string   // required
	Body   string
	Draft  bool
	Labels []string // LABELS_UNSUPPORTED where Capabilities.Labels is false
}

type ProposalUpdate struct {
	Title        *string
	Body         *string
	Draft        *bool
	AddLabels    []string
	RemoveLabels []string
}

type ProposalFilter struct {
	State  ProposalState // "" means open
	Source string
	Target string
	Labels []string
	Page
}

type Page struct {
	Limit int // 0 means the provider's default; never unbounded
	Page  int // 1-based
}

type ProposalList struct {
	Proposals []Proposal
	Truncated bool // more exist beyond Limit
}

type CommentKind string

const (
	CommentDiscussion    CommentKind = "discussion"     // on the proposal
	CommentReview        CommentKind = "review"         // a review summary
	CommentReviewComment CommentKind = "review_comment" // inline, carries Path/Line
)

type Comment struct {
	ID        string
	Kind      CommentKind
	Author    Actor
	Body      string
	Path      string // review_comment only
	Line      int    // review_comment only
	URL       string
	CreatedAt time.Time
}

type CommentList struct {
	Comments  []Comment
	Truncated bool
}

type Commit struct {
	Revision string
	Message  string
	Author   Identity
	When     time.Time
}

type CommitList struct {
	Commits   []Commit
	Truncated bool
}

// ---- errors -------------------------------------------------------------

type Code string

const (
	// Identity and configuration.
	CodeConfigInvalid     Code = "CONFIG_INVALID"     // forge or catalog entry refused
	CodeRepoInvalid       Code = "REPO_INVALID"       // ParseRepoRef refused the value
	CodeRepoUnknown       Code = "REPO_UNKNOWN"       // not in repositories; add it with a role
	CodeForgeUnsupported  Code = "FORGE_UNSUPPORTED"  // no forge serves this host / verb / provider
	CodeRoleVCSVerbs      Code = "ROLE_VCS_VERBS"     // RoleContext refused a mutating verb

	// Workspace guards (owned by #3; codes fixed here).
	CodeTargetIsBranch    Code = "TARGET_IS_BRANCH"   // branch is the base; pick a branch under the prefix
	CodeBranchNotOurs     Code = "BRANCH_NOT_OURS"    // not created in this process / lacks prefix; clone again
	CodeBaseMoved         Code = "BASE_MOVED"         // base rewritten under the target; clone again
	CodeBranchMoved       Code = "BRANCH_MOVED"       // remote tip is not our last published tip
	CodeBranchDiverged    Code = "BRANCH_DIVERGED"    // remote branch exists with history we do not hold
	CodeProtectedBranch   Code = "PROTECTED_BRANCH"   // forge protects the branch against the bot
	CodeBaseBranchMissing Code = "BASE_BRANCH_MISSING"// configured base is not on the remote
	CodeCloneTooLarge     Code = "CLONE_TOO_LARGE"    // tree exceeds the ceiling named in Detail
	CodeNothingToCommit   Code = "NOTHING_TO_COMMIT"  // empty tree or no change
	CodeGitFailed         Code = "GIT_FAILED"         // go-git failure the workspace could not classify

	// Forge calls.
	CodeForgeUnauthorized    Code = "FORGE_UNAUTHORIZED"    // 401 / unthrottled 403; the credential was refused, stop
	CodeForgeUnavailable     Code = "FORGE_UNAVAILABLE"     // 5xx / throttled; retry the same call later
	CodeForgeRejected        Code = "FORGE_REJECTED"        // the forge refused the request; fix it, do not repeat
	CodeForgeCallFailed      Code = "FORGE_CALL_FAILED"     // anything else; forge message verbatim in Detail
	CodeForgeTLSUntrusted    Code = "FORGE_TLS_UNTRUSTED"   // certificate did not verify; names CAFile
	CodeForgeIdentityUnknown Code = "FORGE_IDENTITY_UNKNOWN"// credential cannot name its login
	CodeLabelsUnsupported    Code = "LABELS_UNSUPPORTED"    // Labels on a forge without them
)

func (c Code) Retryable() bool // FORGE_UNAVAILABLE only

type Error struct {
	Code   Code
	Forge  string // Forge.Name, "" for pre-forge failures (parse)
	Verb   Verb   // "" where no verb applies
	Detail string // cause, then the action that follows from it
	cause  error
}

func (e *Error) Error() string // "CODE: detail" (+ " [forge/verb]" when set)
func (e *Error) Unwrap() error
func (e *Error) WithVerb(v Verb) *Error
func Errorf(code Code, forge string, format string, args ...any) *Error // %w wraps
func CodeOf(err error) Code       // "" when err is not a *Error
func IsCode(err error, c Code) bool

// ---- Workspace: go-git working copy (#3) --------------------------------

const (
	DefaultMaxCloneBytes int64 = 256 << 20
	DefaultBranchPrefix        = "agent/"
)

type Policy struct {
	MaxCloneBytes int64
	BranchPrefix  string
}

type FileStatus struct{ Path, Staging, Worktree string }

type Status struct {
	Branch   string
	Revision string
	Clean    bool
	Files    []FileStatus
}

type DiffOptions struct {
	AgainstBase bool
	Paths       []string
}

type PublishResult struct{ Branch, Revision string }

type Workspace struct{ /* unexported */ }

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

### Where each AGENTS.md rule is enforced

| Rule | Mechanism |
|---|---|
| Neutral vocabulary in everything exported | `vcs/vocabulary_test.go`: go/ast over every non-`providers/` package; exported identifiers and `Code`/`Verb` string values must not match `(?i)pull.?request|merge.?request|\bPRs?\b|\bMRs?\b` |
| Agent never writes the target branch | #3's guard tests; the codes and `Verb.Mutating` are fixed here |
| Only the remote half is abstracted | `Provider` has no `Status`/`Diff`/`Log`/`Commit`; the vocabulary test asserts the interface's method set |
| One directory per provider, one registry line | `providers/providers.go` holds `NewRegistry()`, the table; `internal/vcstest.Boundary(t, name, markers)` walks the module and fails any `.go` file outside `providers/<name>/` whose string literals or comments match a marker (import paths excluded, so the module path does not trip `github.com`) |
| Thin clients over `net/http` | `go.mod` review; `verify-slim-deps` in core-agent |
| Credentials are values | `Credential` + redacting constructors; a test formats a `Forge` with `%v`, `%+v`, `%#v` and `%s` and greps for the token |
| Errors carry a code | `CodeOf` + the contract test's "no un-coded error" assertion |

### The contract test (`internal/vcstest`)

```go
type Case struct {
	Name     string                       // "github.com", "ghes", "gitlab-self-managed:private-ca"
	Forge    vcs.Forge                    // the forge under test
	Provider vcs.Constructor
	Accept   map[string]string            // value → canonical "host/path"
	Reject   []string                     // values Resolve must refuse (grammar)
	Noun     string                       // expected ProposalNoun
	Serving  []vcs.Verb                   // verbs the fixture answers
	Induce   map[vcs.Code]func(t testing.TB, repo vcs.Repo) error // one per code the provider can emit
}

func Run(t *testing.T, c Case)
```

`Run` checks, per case: the registry name matches `Name()`; `Hosts` is
non-empty and canonical-first; every `Accept` value resolves through a
`Catalog` to its canonical form and every `Reject` fails `REPO_INVALID`;
traversal and leading-dash segments are refused before the provider sees
them; `Capabilities` lists exactly `Serving` and reports the rest `Missing`;
each `Missing` verb returns `FORGE_UNSUPPORTED` without a network call; each
`Serving` verb round-trips against the fixture and returns typed, non-zero
results; every `Induce` entry yields its code, a non-empty `Detail` and
`Forge == c.Forge.Name`; and no error returned anywhere fails `errors.As`
to `*vcs.Error`. The role guard is tested once in `vcs`, not per provider,
because providers never see it.

`internal/fakeprovider` is an in-memory `Provider` that stores proposals in
a map and can be told to fail the next call with a given code. It is what
#2's `Run` is proven against; #4 and #5 add real fixtures.

### Workspace state (what #3 will build on)

The guards `TARGET_IS_BRANCH`, `BRANCH_NOT_OURS`, `BASE_MOVED` and
`BRANCH_MOVED` are answered from state the model cannot reach. The working
directory is inside the model's path scope — `.git/` included — so nothing
under it may be trusted as a record of what *this process* did. The design
is:

```go
type Workspace struct {
	repo  Repo
	dir   string
	git   *git.Repository        // go-git handle on dir

	clonedBranch   string        // the base branch Clone checked out
	clonedRevision string        // its tip at Clone time — the BASE_MOVED anchor

	mu   sync.Mutex
	ours map[string]*branchRecord // branches Branch() created in this process
}

type branchRecord struct {
	base          string // revision the branch was cut from
	lastPublished string // our last pushed tip; "" before first publish
}
```

- **There is no `Open(dir)`, no `Load`, no `MarshalJSON`.** The only
  constructor is `Clone`. After a process restart the directory is still
  there but no `Workspace` is, so `Publish` on any branch is
  `BRANCH_NOT_OURS` ("clone again") — the model cannot manufacture a record
  that says a branch is ours because no code path reads one.
- **`Publish(ctx, branch)` takes no target** (decision 15). The implicit
  target is `clonedBranch`, which is the base (`Clone` checks out
  `BaseBranchName`). `TARGET_IS_BRANCH` is
  `branch == clonedBranch || branch == BaseBranch`.
- `BASE_MOVED` fetches the remote target's tip (one `ls-remote` plus a
  single-ref fetch into a remote-tracking ref), then asks go-git whether
  `clonedRevision` is an ancestor of it — base under target, first publish
  only (`lastPublished == ""`). Objects the model tampered with locally can
  only make the push fail on the remote's receive side; the decision
  itself compares a revision held in memory with one the remote just
  served.
- `BRANCH_MOVED` compares the remote's current tip of `branch` with
  `lastPublished` on every publish after the first; the push refspec is
  never forced.
- Per-process bookkeeping across tool calls is the consumer's: core-agent
  keeps a `map[dir]*Workspace` for the daemon's lifetime. The core may ship
  a small `vcs.Workspaces` set for that if #3 finds every consumer writing
  the same ten lines.
- `BranchPrefix` and `MaxCloneBytes` arrive as a `vcs.Policy` the
  `Workspace` is created with; they are not on `Forge` or `Repo` because
  they are per-install, not per-forge.

## Resolved questions

Settled 2026-11-04 in review; recorded as decisions 13–15.

13. **`FORGE_UNAUTHORIZED` is its own code.** A 401, or a 403 without
    rate-limit headers, means the credential was refused: the next action
    is "stop and report", not "fix the request" (`FORGE_REJECTED`, the 422
    code) and not "retry later" (`FORGE_UNAVAILABLE`, which a throttled 403
    still maps to). Issue #5's `401/403 → FORGE_REJECTED` line is
    superseded by this.

14. **A repository on a configured forge that is not in `repositories` is
    refused, `REPO_UNKNOWN`.** The repositories list is the allowlist of
    what the install's token may be spent on, not a hint. A consumer that
    wants an unlisted repository calls `Catalog.NewRepo(RepoConfig)` in Go,
    with a role it chose; that is the one door and it is visible in code
    review rather than in a config default.

15. **`Workspace.Publish(ctx, branch)` takes no target and no options.**
    The only target the workspace can reason about is the branch `Clone`
    checked out, because that is the only one whose revision it recorded.
    `TARGET_IS_BRANCH` is `branch == clonedBranch || branch ==
    repo.BaseBranch`; `BASE_MOVED` is anchored on `clonedRevision`. The
    forge-side target — what a proposal merges into — is
    `ProposalRequest.Target`, which defaults to the base. There is no
    `advance` and no `TARGET_NOT_BASE`: a second publish is a publish whose
    `lastPublished` is set and is checked by `BRANCH_MOVED`. Issue #3's
    `Publish(ctx, branch, target, opts)` is superseded by this.
