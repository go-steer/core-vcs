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

import (
	"context"
	"time"
)

// Provider is what one package per forge implements. A consumer never
// calls a Provider; it calls Repo, which checks the role and the forge's
// verb set and then delegates here. Everything a Provider returns is in
// the neutral vocabulary: a provider translates the forge's model to
// these types and its HTTP statuses to Codes, with the forge's own
// message in Detail.
//
// A Provider is constructed once per Catalog and may be used from many
// goroutines.
type Provider interface {
	// Name is the registry name, e.g. "github".
	Name() string
	// ValidateForge applies the provider's own configuration rules: a
	// CAFile on a public host, a missing Host, a scheme it does not admit.
	ValidateForge(f Forge) error
	// Hosts returns every host spelling f answers to, canonical first. It
	// is never empty. The core uses it for host matching and for the
	// schemeless host/path parse; no file outside the provider's own
	// directory names the forge's hosts.
	Hosts(f Forge) []string
	// Resolve applies the provider's path grammar and returns the
	// canonical ref. ref.Host is already set and already matched to f.
	// Failure is REPO_INVALID.
	Resolve(f Forge, ref RepoRef) (RepoRef, error)

	// Capabilities answers the forge-static half: ProposalNoun, Verbs and
	// Labels. No network. The core fills BaseBranch and Missing.
	Capabilities(repo Repo) Capabilities
	// CloneURL is the HTTPS URL a Workspace clones from, with no
	// credential in it.
	CloneURL(repo Repo) string
	// CloneAuth is what the smart-HTTP transport presents. nil means
	// anonymous.
	CloneAuth(ctx context.Context, repo Repo) (*BasicAuth, error)
	// BaseBranch is the remote's default branch, asked only when the
	// repository configured none.
	BaseBranch(ctx context.Context, repo Repo) (string, error)
	// ProtectedBranch reports whether the forge protects branch against
	// the credential. false where the forge cannot say.
	ProtectedBranch(ctx context.Context, repo Repo, branch string) (bool, error)

	CreateProposal(ctx context.Context, repo Repo, req ProposalRequest) (Proposal, error)
	ListProposals(ctx context.Context, repo Repo, f ProposalFilter) (ProposalList, error)
	GetProposal(ctx context.Context, repo Repo, number int) (Proposal, error)
	ProposalComments(ctx context.Context, repo Repo, number int, p Page) (CommentList, error)
	ProposalCommits(ctx context.Context, repo Repo, number int, p Page) (CommitList, error)
	CommentProposal(ctx context.Context, repo Repo, number int, body string) (Comment, error)
	UpdateProposal(ctx context.Context, repo Repo, number int, u ProposalUpdate) (Proposal, error)
	CloseProposal(ctx context.Context, repo Repo, number int) (Proposal, error)
}

// Constructor builds a Provider. The registry maps a name to one.
type Constructor func() (Provider, error)

// ProposalNoun values the core defines. A forge with a review object
// supplies its own noun instead ("pull request", "merge request").
const (
	// ProposalNounMissing: the forge has no proposal; the published branch
	// is the deliverable.
	ProposalNounMissing = "missing"
	// ProposalNounLink: CreateProposal returns a URL a human opens and no
	// number; nothing else about the proposal can be read or written.
	ProposalNounLink = "link"
)

// Capabilities is the first thing a caller asks: what this forge calls a
// proposal, which verbs it serves, and what to target.
type Capabilities struct {
	// ProposalNoun is the forge's word, or ProposalNounLink / ProposalNounMissing.
	ProposalNoun string
	// BaseBranch is the effective base: configured, else the remote's.
	BaseBranch string
	// Verbs are the proposal verbs this forge serves.
	Verbs []Verb
	// Missing are the proposal verbs it does not; each returns
	// FORGE_UNSUPPORTED without a network call.
	Missing []Verb
	// Labels reports whether proposals carry labels here.
	Labels bool
}

// Serves reports whether v is in Verbs.
func (c Capabilities) Serves(v Verb) bool {
	for _, s := range c.Verbs {
		if s == v {
			return true
		}
	}
	return false
}

// ProposalState is one of three outcomes on every forge, however a forge
// encodes the difference between closed and merged.
type ProposalState string

const (
	ProposalOpen   ProposalState = "open"
	ProposalClosed ProposalState = "closed"
	ProposalMerged ProposalState = "merged"
)

// Actor is who did something on the forge.
type Actor struct {
	// Login is the forge login with automation suffixes ("[bot]") removed.
	Login string
	// Bot is the forge's own answer to whether the actor is an automation.
	Bot bool
}

// Proposal is a change offered for review.
type Proposal struct {
	// Number is the forge's identifier; 0 under ProposalNounLink.
	Number int
	URL    string
	Title  string
	Body   string
	State  ProposalState
	// Source is the branch under review; Target is the branch it merges into.
	Source string
	Target string
	// Revision is the source tip as the forge last saw it.
	Revision  string
	Draft     bool
	Author    Actor
	Labels    []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ProposalRequest opens a proposal.
type ProposalRequest struct {
	// Source is the published branch. Required.
	Source string
	// Target is the branch to merge into. Empty means the repository's
	// base; Repo fills it before the provider sees the request.
	Target string
	// Title is required.
	Title string
	Body  string
	Draft bool
	// Labels is refused with LABELS_UNSUPPORTED where Capabilities.Labels
	// is false.
	Labels []string
}

// ProposalUpdate edits a proposal. A nil pointer leaves the field alone.
type ProposalUpdate struct {
	Title        *string
	Body         *string
	Draft        *bool
	AddLabels    []string
	RemoveLabels []string
}

// Page bounds a list. A provider never returns an unbounded list.
type Page struct {
	// Limit is the most items to return; 0 means the provider's default.
	Limit int
	// Page is 1-based; 0 means the first.
	Page int
}

// ProposalFilter narrows ListProposals.
type ProposalFilter struct {
	// State filters by outcome; "" means ProposalOpen.
	State ProposalState
	// Source and Target filter by branch; "" means any.
	Source string
	Target string
	// Labels requires every listed label.
	Labels []string
	Page
}

// ProposalList is one page of proposals.
type ProposalList struct {
	Proposals []Proposal
	// Truncated reports that more exist beyond this page.
	Truncated bool
}

// CommentKind says where on the forge a comment lives.
type CommentKind string

const (
	// CommentDiscussion is on the proposal itself.
	CommentDiscussion CommentKind = "discussion"
	// CommentReview is a review's summary.
	CommentReview CommentKind = "review"
	// CommentReviewComment is inline on a diff and carries Path and Line.
	CommentReviewComment CommentKind = "review_comment"
)

// Comment is one entry in a proposal's discussion.
type Comment struct {
	ID        string
	Kind      CommentKind
	Author    Actor
	Body      string
	Path      string // CommentReviewComment only
	Line      int    // CommentReviewComment only
	URL       string
	CreatedAt time.Time
}

// CommentList is one page of comments.
type CommentList struct {
	Comments  []Comment
	Truncated bool
}

// Commit is one revision as the forge reports it.
type Commit struct {
	Revision string
	Message  string
	Author   Identity
	When     time.Time
}

// CommitList is one page of commits.
type CommitList struct {
	Commits   []Commit
	Truncated bool
}
