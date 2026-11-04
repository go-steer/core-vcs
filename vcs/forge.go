// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vcs

// Verb names one operation in the neutral vocabulary. Local verbs run on
// the working copy; proposal verbs cross to the forge through a Provider.
type Verb string

const (
	// VerbClone materialises a working copy. Read-class.
	VerbClone Verb = "clone"
	// VerbCommit records a revision locally.
	VerbCommit Verb = "commit"
	// VerbPublish sends a branch to the remote under every guard.
	VerbPublish Verb = "publish"

	// VerbProposalCreate offers a published branch for review.
	VerbProposalCreate Verb = "proposal-create"
	// VerbProposalList lists proposals.
	VerbProposalList Verb = "proposal-list"
	// VerbProposalView reads one proposal.
	VerbProposalView Verb = "proposal-view"
	// VerbProposalComments reads a proposal's discussion.
	VerbProposalComments Verb = "proposal-comments"
	// VerbProposalComment adds to a proposal's discussion.
	VerbProposalComment Verb = "proposal-comment"
	// VerbProposalUpdate edits a proposal's title, body, draft flag or labels.
	VerbProposalUpdate Verb = "proposal-update"
	// VerbProposalClose closes a proposal without merging it.
	VerbProposalClose Verb = "proposal-close"
	// VerbProposalCommits lists the revisions a proposal carries.
	VerbProposalCommits Verb = "proposal-commits"
)

// ProposalVerbs is every verb that goes through a Provider, in a fixed
// order. Capabilities.Missing is computed against it.
var ProposalVerbs = []Verb{
	VerbProposalCreate, VerbProposalList, VerbProposalView, VerbProposalComments,
	VerbProposalComment, VerbProposalUpdate, VerbProposalClose, VerbProposalCommits,
}

// Mutating reports whether v writes anywhere: the working copy, the remote
// or the forge. A RoleContext repository refuses every mutating verb.
func (v Verb) Mutating() bool {
	switch v {
	case VerbCommit, VerbPublish, VerbProposalCreate, VerbProposalComment,
		VerbProposalUpdate, VerbProposalClose:
		return true
	default:
		return false
	}
}

// Role is what the consumer declared it intends to do with a repository.
type Role string

const (
	// RoleGitOps is the one repository proposals are opened on. At most one
	// per Catalog.
	RoleGitOps Role = "gitops"
	// RoleManaged is read-write.
	RoleManaged Role = "managed"
	// RoleContext is read-only: clone and every read verb work, every
	// mutating verb returns ROLE_VCS_VERBS.
	RoleContext Role = "context"
)

// Allows reports whether a repository in role r may run v. An unknown
// role allows nothing.
func (r Role) Allows(v Verb) bool {
	switch r {
	case RoleGitOps, RoleManaged:
		return true
	case RoleContext:
		return !v.Mutating()
	default:
		return false
	}
}

// Valid reports whether r is one of the three roles.
func (r Role) Valid() bool {
	return r == RoleGitOps || r == RoleManaged || r == RoleContext
}

// Identity is a commit author or committer.
type Identity struct {
	Name  string
	Email string
}

// Forge is one entry in the consumer's forges list: a host a provider
// speaks to, with the credential that host takes. A Forge is a value; it
// is never mutated after NewCatalog has seen it.
type Forge struct {
	// Name is unique within a Catalog and is what RepoConfig.Forge names.
	Name string
	// Provider is the registry name: "github", "gitlab", "git", ...
	Provider string
	// Host is the forge's host. Empty means the provider's public host;
	// a provider with no default host requires it.
	Host string
	// Scheme is "" or "https". A provider may admit "http" for a test
	// remote; none does by default.
	Scheme string
	// Port is the API and clone port; 0 means the scheme's default.
	Port int
	// CAFile adds root certificates for a self-managed host. TLSConfig
	// resolves it once; a provider never reads it.
	CAFile string
	// Username is the clone basic-auth user for the "git" provider.
	// Providers with a fixed user ignore it.
	Username string
	// ProposalURLTemplate, on the "git" provider, turns a published branch
	// into a link: {host} {path} {base} {branch} are substituted.
	ProposalURLTemplate string
	// CommitIdentity is the author and committer of every commit the
	// Workspace records on this forge's repositories.
	CommitIdentity Identity
	// Cred supplies the token at call time. nil is anonymous, legal only
	// where the provider says so.
	Cred Credential
}

// RepoConfig is one entry in the consumer's repositories list.
type RepoConfig struct {
	// Forge names a Forge by Forge.Name.
	Forge string
	// Repository is anything ParseRepoRef accepts: a bare path, host/path,
	// or a URL on the forge's host.
	Repository string
	// Role is what the consumer intends to do here.
	Role Role
	// BaseBranch is the branch proposals target. Empty means ask the
	// provider for the remote's default. Refused on RoleContext.
	BaseBranch string
}
