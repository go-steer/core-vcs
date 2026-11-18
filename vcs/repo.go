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
	"errors"
	"strings"
)

// Repo is the consumer-facing handle to one repository on one forge. It
// is built by Catalog.Resolve or Catalog.NewRepo and never by a struct
// literal: its provider is unexported, so every verb runs the role guard,
// the capability guard and the label guard here before the provider sees
// the call.
type Repo struct {
	Forge      Forge
	Ref        RepoRef // canonical; Host is always set
	Role       Role
	BaseBranch string // configured; "" until BaseBranchName asks the remote

	provider Provider
}

// String renders the canonical "host/path".
func (r Repo) String() string { return r.Ref.String() }

// ProviderName is the registry name of the forge's provider.
func (r Repo) ProviderName() string {
	if r.provider == nil {
		return ""
	}
	return r.provider.Name()
}

// CloneURL is the HTTPS URL a Workspace clones from, with no credential.
func (r Repo) CloneURL() string {
	if r.provider == nil {
		return ""
	}
	return r.provider.CloneURL(r)
}

// CloneAuth is what the smart-HTTP transport presents when cloning or
// publishing r. nil is anonymous.
func (r Repo) CloneAuth(ctx context.Context) (*BasicAuth, error) {
	if err := r.ready(VerbClone); err != nil {
		return nil, err
	}
	auth, err := r.provider.CloneAuth(ctx, r)
	return auth, r.wrap(VerbClone, err)
}

// ProtectedBranch reports whether the forge protects branch against the
// credential. false where the forge cannot say.
func (r Repo) ProtectedBranch(ctx context.Context, branch string) (bool, error) {
	if err := r.ready(VerbPublish); err != nil {
		return false, err
	}
	branch = strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
	if branch == "" {
		return false, Errorf(CodeForgeRejected, r.Forge.Name, "branch name is empty").WithVerb(VerbPublish)
	}
	ok, err := r.provider.ProtectedBranch(ctx, r, branch)
	return ok, r.wrap(VerbPublish, err)
}

// BaseBranchName returns r.BaseBranch when configured, or asks the
// provider for the remote's default branch.
func (r Repo) BaseBranchName(ctx context.Context) (string, error) {
	if r.BaseBranch != "" {
		return r.BaseBranch, nil
	}
	if r.provider == nil {
		return "", Errorf(CodeConfigInvalid, r.Forge.Name, "repository %s was not built by a Catalog", r.Ref)
	}
	b, err := r.provider.BaseBranch(ctx, r)
	if err != nil {
		return "", r.wrap("", err)
	}
	b = strings.TrimSpace(strings.TrimPrefix(b, "refs/heads/"))
	if b == "" {
		return "", Errorf(CodeBaseBranchMissing, r.Forge.Name, "remote reported an empty default branch for %s", r.Ref)
	}
	return b, nil
}

// Capabilities returns what this forge calls a proposal, which verbs it
// serves, which are missing, and the effective base branch.
func (r Repo) Capabilities(ctx context.Context) (Capabilities, error) {
	if r.provider == nil {
		return Capabilities{}, Errorf(CodeConfigInvalid, r.Forge.Name, "repository %s was not built by a Catalog", r.Ref)
	}
	base, err := r.BaseBranchName(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	caps := r.staticCapabilities()
	caps.BaseBranch = base
	return caps, nil
}

func (r Repo) staticCapabilities() Capabilities {
	caps := r.provider.Capabilities(r)
	if caps.ProposalNoun == "" {
		caps.ProposalNoun = ProposalNounMissing
	}
	_, canReview := r.provider.(ProposalReviewer)
	_, canMerge := r.provider.(ProposalMerger)
	if len(caps.Verbs) > 0 && (!canReview || !canMerge) {
		filtered := make([]Verb, 0, len(caps.Verbs))
		for _, v := range caps.Verbs {
			if v == VerbProposalReview && !canReview {
				continue
			}
			if v == VerbProposalMerge && !canMerge {
				continue
			}
			filtered = append(filtered, v)
		}
		caps.Verbs = filtered
	}
	var missing []Verb
	for _, v := range ProposalVerbs {
		if !caps.Serves(v) {
			missing = append(missing, v)
		}
	}
	caps.Missing = missing
	return caps
}

// CreateProposal opens a proposal. If req.Target is empty it is filled
// from BaseBranchName; if r.BaseBranch is pinned, any other target is
// refused before the forge is called.
func (r Repo) CreateProposal(ctx context.Context, req ProposalRequest) (Proposal, error) {
	caps, err := r.gate(VerbProposalCreate, len(req.Labels) > 0)
	if err != nil {
		return Proposal{}, err
	}
	req.Source = strings.TrimPrefix(strings.TrimSpace(req.Source), "refs/heads/")
	req.Target = strings.TrimPrefix(strings.TrimSpace(req.Target), "refs/heads/")
	req.Title = strings.TrimSpace(req.Title)
	if req.Source == "" {
		return Proposal{}, Errorf(CodeForgeRejected, r.Forge.Name, "source branch is empty; publish a branch first").WithVerb(VerbProposalCreate)
	}
	if err := checkBranchName(r.Forge.Name, req.Source); err != nil {
		return Proposal{}, r.wrap(VerbProposalCreate, err)
	}
	if req.Target != "" {
		if err := checkBranchName(r.Forge.Name, req.Target); err != nil {
			return Proposal{}, r.wrap(VerbProposalCreate, err)
		}
	}
	if req.Title == "" && caps.ProposalNoun != ProposalNounLink {
		return Proposal{}, Errorf(CodeForgeRejected, r.Forge.Name, "proposal title is empty").WithVerb(VerbProposalCreate)
	}
	base, err := r.BaseBranchName(ctx)
	if err != nil {
		return Proposal{}, r.wrap(VerbProposalCreate, err)
	}
	if req.Target == "" {
		req.Target = base
	}
	if r.BaseBranch != "" && req.Target != r.BaseBranch {
		return Proposal{}, Errorf(CodeForgeRejected, r.Forge.Name, "target %q is not the configured base branch %q", req.Target, r.BaseBranch).WithVerb(VerbProposalCreate)
	}
	if req.Source == req.Target {
		return Proposal{}, Errorf(CodeTargetIsBranch, r.Forge.Name, "source branch %q is the target branch; propose from a topic branch", req.Source).WithVerb(VerbProposalCreate)
	}
	p, err := r.provider.CreateProposal(ctx, r, req)
	return p, r.wrap(VerbProposalCreate, err)
}

// ListProposals returns one page of proposals matching f.
func (r Repo) ListProposals(ctx context.Context, f ProposalFilter) (ProposalList, error) {
	if _, err := r.gate(VerbProposalList, len(f.Labels) > 0); err != nil {
		return ProposalList{}, err
	}
	if f.State == "" {
		f.State = ProposalOpen
	}
	out, err := r.provider.ListProposals(ctx, r, f)
	return out, r.wrap(VerbProposalList, err)
}

// GetProposal reads one proposal by number.
func (r Repo) GetProposal(ctx context.Context, number int) (Proposal, error) {
	if _, err := r.gate(VerbProposalView, false); err != nil {
		return Proposal{}, err
	}
	if err := checkNumber(r.Forge.Name, VerbProposalView, number); err != nil {
		return Proposal{}, err
	}
	p, err := r.provider.GetProposal(ctx, r, number)
	return p, r.wrap(VerbProposalView, err)
}

// ProposalComments reads one page of a proposal's discussion.
func (r Repo) ProposalComments(ctx context.Context, number int, p Page) (CommentList, error) {
	if _, err := r.gate(VerbProposalComments, false); err != nil {
		return CommentList{}, err
	}
	if err := checkNumber(r.Forge.Name, VerbProposalComments, number); err != nil {
		return CommentList{}, err
	}
	out, err := r.provider.ProposalComments(ctx, r, number, p)
	return out, r.wrap(VerbProposalComments, err)
}

// ProposalCommits reads one page of the revisions a proposal carries.
func (r Repo) ProposalCommits(ctx context.Context, number int, p Page) (CommitList, error) {
	if _, err := r.gate(VerbProposalCommits, false); err != nil {
		return CommitList{}, err
	}
	if err := checkNumber(r.Forge.Name, VerbProposalCommits, number); err != nil {
		return CommitList{}, err
	}
	out, err := r.provider.ProposalCommits(ctx, r, number, p)
	return out, r.wrap(VerbProposalCommits, err)
}

// CommentProposal adds a comment to a proposal.
func (r Repo) CommentProposal(ctx context.Context, number int, body string) (Comment, error) {
	if _, err := r.gate(VerbProposalComment, false); err != nil {
		return Comment{}, err
	}
	if err := checkNumber(r.Forge.Name, VerbProposalComment, number); err != nil {
		return Comment{}, err
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return Comment{}, Errorf(CodeForgeRejected, r.Forge.Name, "comment body is empty").WithVerb(VerbProposalComment)
	}
	c, err := r.provider.CommentProposal(ctx, r, number, body)
	return c, r.wrap(VerbProposalComment, err)
}

// UpdateProposal edits a proposal's title, body, draft flag or labels.
func (r Repo) UpdateProposal(ctx context.Context, number int, u ProposalUpdate) (Proposal, error) {
	hasLabels := len(u.AddLabels) > 0 || len(u.RemoveLabels) > 0
	if _, err := r.gate(VerbProposalUpdate, hasLabels); err != nil {
		return Proposal{}, err
	}
	if err := checkNumber(r.Forge.Name, VerbProposalUpdate, number); err != nil {
		return Proposal{}, err
	}
	p, err := r.provider.UpdateProposal(ctx, r, number, u)
	return p, r.wrap(VerbProposalUpdate, err)
}

// CloseProposal closes a proposal without merging it.
func (r Repo) CloseProposal(ctx context.Context, number int) (Proposal, error) {
	if _, err := r.gate(VerbProposalClose, false); err != nil {
		return Proposal{}, err
	}
	if err := checkNumber(r.Forge.Name, VerbProposalClose, number); err != nil {
		return Proposal{}, err
	}
	p, err := r.provider.CloseProposal(ctx, r, number)
	return p, r.wrap(VerbProposalClose, err)
}

// ReviewProposal records a structured review decision (approve,
// request_changes, or comment) on an open proposal.
func (r Repo) ReviewProposal(ctx context.Context, number int, req ProposalReviewRequest) (ProposalReview, error) {
	caps, err := r.gate(VerbProposalReview, false)
	if err != nil {
		return ProposalReview{}, err
	}
	reviewer, ok := r.provider.(ProposalReviewer)
	if !ok {
		return ProposalReview{}, Errorf(CodeForgeUnsupported, r.Forge.Name, "forge %q (%s) does not support %s (proposalNoun: %s)", r.Forge.Name, r.provider.Name(), VerbProposalReview, caps.ProposalNoun).WithVerb(VerbProposalReview)
	}
	if err := checkNumber(r.Forge.Name, VerbProposalReview, number); err != nil {
		return ProposalReview{}, err
	}
	switch req.Decision {
	case ReviewApprove, ReviewRequestChanges, ReviewComment:
	default:
		return ProposalReview{}, Errorf(CodeForgeRejected, r.Forge.Name, "review decision %q must be %q, %q, or %q", req.Decision, ReviewApprove, ReviewRequestChanges, ReviewComment).WithVerb(VerbProposalReview)
	}
	if req.ExpectedRevision != "" {
		trimmedRev := strings.TrimSpace(req.ExpectedRevision)
		if trimmedRev == "" {
			return ProposalReview{}, Errorf(CodeForgeRejected, r.Forge.Name, "expected revision cannot be whitespace").WithVerb(VerbProposalReview)
		}
		req.ExpectedRevision = trimmedRev
	}
	if len(req.Comments) > 0 {
		cleaned := make([]ReviewLineComment, len(req.Comments))
		for i, c := range req.Comments {
			p := strings.TrimSpace(c.Path)
			if p == "" {
				return ProposalReview{}, Errorf(CodeForgeRejected, r.Forge.Name, "review line comment %d path is empty", i).WithVerb(VerbProposalReview)
			}
			if c.Line <= 0 {
				return ProposalReview{}, Errorf(CodeForgeRejected, r.Forge.Name, "review line comment %d line %d must be positive", i, c.Line).WithVerb(VerbProposalReview)
			}
			b := strings.TrimSpace(c.Body)
			if b == "" {
				return ProposalReview{}, Errorf(CodeForgeRejected, r.Forge.Name, "review line comment %d body is empty", i).WithVerb(VerbProposalReview)
			}
			cleaned[i] = ReviewLineComment{Path: p, Line: c.Line, Body: b}
		}
		req.Comments = cleaned
	}
	req.Body = strings.TrimSpace(req.Body)
	if (req.Decision == ReviewRequestChanges || req.Decision == ReviewComment) && req.Body == "" && len(req.Comments) == 0 {
		return ProposalReview{}, Errorf(CodeForgeRejected, r.Forge.Name, "%s review requires a non-empty body or at least one line comment", req.Decision).WithVerb(VerbProposalReview)
	}
	out, err := reviewer.ReviewProposal(ctx, r, number, req)
	return out, r.wrap(VerbProposalReview, err)
}

// MergeProposal merges an open proposal into its target branch, verifying
// req.ExpectedRevision against the proposal's head SHA.
func (r Repo) MergeProposal(ctx context.Context, number int, req ProposalMergeRequest) (ProposalMergeResult, error) {
	caps, err := r.gate(VerbProposalMerge, false)
	if err != nil {
		return ProposalMergeResult{}, err
	}
	merger, ok := r.provider.(ProposalMerger)
	if !ok {
		return ProposalMergeResult{}, Errorf(CodeForgeUnsupported, r.Forge.Name, "forge %q (%s) does not support %s (proposalNoun: %s)", r.Forge.Name, r.provider.Name(), VerbProposalMerge, caps.ProposalNoun).WithVerb(VerbProposalMerge)
	}
	if err := checkNumber(r.Forge.Name, VerbProposalMerge, number); err != nil {
		return ProposalMergeResult{}, err
	}
	req.ExpectedRevision = strings.TrimSpace(req.ExpectedRevision)
	if req.ExpectedRevision == "" {
		return ProposalMergeResult{}, Errorf(CodeForgeRejected, r.Forge.Name, "expected revision is required to merge a proposal").WithVerb(VerbProposalMerge)
	}
	switch req.Method {
	case MergeDefault, MergeCreateCommit, MergeSquash, MergeRebase:
	default:
		return ProposalMergeResult{}, Errorf(CodeForgeRejected, r.Forge.Name, "merge method %q must be empty, %q, %q, or %q", req.Method, MergeCreateCommit, MergeSquash, MergeRebase).WithVerb(VerbProposalMerge)
	}
	req.CommitTitle = strings.TrimSpace(req.CommitTitle)
	req.CommitMessage = strings.TrimSpace(req.CommitMessage)
	out, err := merger.MergeProposal(ctx, r, number, req)
	return out, r.wrap(VerbProposalMerge, err)
}

func (r Repo) ready(v Verb) error {
	if r.provider == nil {
		return Errorf(CodeConfigInvalid, r.Forge.Name, "repository %s was not built by a Catalog", r.Ref).WithVerb(v)
	}
	if !r.Role.Allows(v) {
		return Errorf(CodeRoleVCSVerbs, r.Forge.Name, "%s has role %q (read-only); %s is a mutating verb", r.Ref, r.Role, v).WithVerb(v)
	}
	return nil
}

func (r Repo) gate(v Verb, usesLabels bool) (Capabilities, error) {
	if err := r.ready(v); err != nil {
		return Capabilities{}, err
	}
	caps := r.staticCapabilities()
	if !caps.Serves(v) {
		return Capabilities{}, Errorf(CodeForgeUnsupported, r.Forge.Name, "forge %q (%s) does not support %s (proposalNoun: %s)", r.Forge.Name, r.provider.Name(), v, caps.ProposalNoun).WithVerb(v)
	}
	if usesLabels && !caps.Labels {
		return Capabilities{}, Errorf(CodeLabelsUnsupported, r.Forge.Name, "forge %q (%s) does not support labels on proposals", r.Forge.Name, r.provider.Name()).WithVerb(v)
	}
	return caps, nil
}

func (r Repo) wrap(v Verb, err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		out := *e
		if out.Forge == "" {
			out.Forge = r.Forge.Name
		}
		if out.Verb == "" {
			out.Verb = v
		}
		if out.Detail == "" {
			out.Detail = string(out.Code)
		}
		return &out
	}
	return TransportError(r.Forge, v, err)
}

func checkNumber(forge string, v Verb, n int) error {
	if n <= 0 {
		return Errorf(CodeForgeRejected, forge, "proposal number %d must be positive", n).WithVerb(v)
	}
	return nil
}
