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

package vcs_test

import (
	"context"
	"testing"

	"github.com/go-steer/core-vcs/internal/fakeprovider"
	"github.com/go-steer/core-vcs/internal/vcstest"
	"github.com/go-steer/core-vcs/vcs"
)

// TestRoleContextGuard verifies that a RoleContext repository refuses
// every mutating verb with ROLE_VCS_VERBS against a live fixture that
// would otherwise accept the call — so commenting out the role check in
// Repo.ready makes this test fail.
func TestRoleContextGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := fakeprovider.NewFixture(t)

	reg := vcs.NewRegistry()
	reg.MustRegister(fakeprovider.Name, fakeprovider.New(fakeprovider.Options{
		Labels:  true,
		DialURL: fx.Server.URL,
	}))
	forge := vcs.Forge{
		Name:     "f",
		Provider: fakeprovider.Name,
		Cred:     vcs.StaticToken(fakeprovider.ValidToken),
	}
	cat, err := vcs.NewCatalog(reg, []vcs.Forge{forge}, []vcs.RepoConfig{
		{Forge: "f", Repository: "acme/rw", Role: vcs.RoleGitOps, BaseBranch: "main"},
		{Forge: "f", Repository: "acme/ro", Role: vcs.RoleContext},
	})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	rw, _ := cat.Resolve("acme/rw")
	ro, _ := cat.Resolve("acme/ro")

	// Seed a proposal on the read-write repo so read verbs on ro have a
	// live fixture and mutating verbs by number have a real target.
	p, err := rw.CreateProposal(ctx, vcs.ProposalRequest{Source: "agent/seed", Title: "seed"})
	if err != nil {
		t.Fatalf("seed CreateProposal: %v", err)
	}

	// Read verbs succeed on RoleContext.
	if _, err := ro.Capabilities(ctx); err != nil {
		t.Fatalf("ro.Capabilities: %v", err)
	}
	if _, err := ro.CloneAuth(ctx); err != nil {
		t.Fatalf("ro.CloneAuth: %v", err)
	}
	if _, err := ro.ListProposals(ctx, vcs.ProposalFilter{}); err != nil {
		t.Fatalf("ro.ListProposals: %v", err)
	}
	if _, err := ro.GetProposal(ctx, p.Number); err != nil {
		t.Fatalf("ro.GetProposal: %v", err)
	}
	if _, err := ro.ProposalComments(ctx, p.Number, vcs.Page{}); err != nil {
		t.Fatalf("ro.ProposalComments: %v", err)
	}
	if _, err := ro.ProposalCommits(ctx, p.Number, vcs.Page{}); err != nil {
		t.Fatalf("ro.ProposalCommits: %v", err)
	}

	// Every mutating verb is refused with ROLE_VCS_VERBS.
	_, err = ro.ProtectedBranch(ctx, "main")
	vcstest.RequireError(t, err, vcs.CodeRoleVCSVerbs, "f")

	_, err = ro.CreateProposal(ctx, vcs.ProposalRequest{Source: "agent/fix", Title: "fix"})
	vcstest.RequireError(t, err, vcs.CodeRoleVCSVerbs, "f")

	_, err = ro.CommentProposal(ctx, p.Number, "hi")
	vcstest.RequireError(t, err, vcs.CodeRoleVCSVerbs, "f")

	title := "edited"
	_, err = ro.UpdateProposal(ctx, p.Number, vcs.ProposalUpdate{Title: &title})
	vcstest.RequireError(t, err, vcs.CodeRoleVCSVerbs, "f")

	_, err = ro.CloseProposal(ctx, p.Number)
	vcstest.RequireError(t, err, vcs.CodeRoleVCSVerbs, "f")

	_, err = ro.ReviewProposal(ctx, p.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: p.Revision,
	})
	vcstest.RequireError(t, err, vcs.CodeRoleVCSVerbs, "f")

	_, err = ro.MergeProposal(ctx, p.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: p.Revision,
	})
	vcstest.RequireError(t, err, vcs.CodeRoleVCSVerbs, "f")
}

// TestCreateProposalGuards verifies the target-is-branch guard and the
// pinned-base-branch guard on Repo.CreateProposal against a live fixture
// that accepts any request, so commenting out either guard fails the test.
func TestCreateProposalGuards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := fakeprovider.NewFixture(t)

	reg := vcs.NewRegistry()
	reg.MustRegister(fakeprovider.Name, fakeprovider.New(fakeprovider.Options{
		Labels:  true,
		DialURL: fx.Server.URL,
	}))
	forge := vcs.Forge{
		Name:     "f",
		Provider: fakeprovider.Name,
		Cred:     vcs.StaticToken(fakeprovider.ValidToken),
	}
	cat, err := vcs.NewCatalog(reg, []vcs.Forge{forge}, []vcs.RepoConfig{
		{Forge: "f", Repository: "acme/pinned", Role: vcs.RoleGitOps, BaseBranch: "refs/heads/production"},
		{Forge: "f", Repository: "acme/unpinned", Role: vcs.RoleManaged},
	})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	pinned, _ := cat.Resolve("acme/pinned")
	unpinned, _ := cat.Resolve("acme/unpinned")

	// ProtectedBranch strips refs/heads/ before asking the forge: the
	// fixture marks "main" protected and would answer false for
	// "refs/heads/main".
	if prot, err := unpinned.ProtectedBranch(ctx, "refs/heads/main"); err != nil || !prot {
		t.Fatalf("ProtectedBranch(refs/heads/main) = (%v, %v), want (true, nil)", prot, err)
	}
	_, err = unpinned.ProtectedBranch(ctx, "refs/heads/")
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")

	// Source == Target is TARGET_IS_BRANCH (both explicit and defaulted).
	_, err = pinned.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "production",
		Title:  "would merge branch into itself",
	})
	vcstest.RequireError(t, err, vcs.CodeTargetIsBranch, "f")

	_, err = unpinned.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "refs/heads/main",
		Title:  "default base is main on fixture",
	})
	vcstest.RequireError(t, err, vcs.CodeTargetIsBranch, "f")

	// Pinned BaseBranch refuses any other Target.
	_, err = pinned.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "agent/fix",
		Target: "main",
		Title:  "wrong target",
	})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")

	// Empty Source or Title is FORGE_REJECTED.
	_, err = pinned.CreateProposal(ctx, vcs.ProposalRequest{Title: "no source"})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")
	_, err = pinned.CreateProposal(ctx, vcs.ProposalRequest{Source: "agent/fix"})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")

	// Empty Target defaults to the pinned BaseBranch and succeeds.
	got, err := pinned.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "refs/heads/agent/fix",
		Title:  "valid",
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if got.Target != "production" || got.Source != "agent/fix" {
		t.Errorf("got Target=%q Source=%q, want production / agent/fix", got.Target, got.Source)
	}

	// Non-positive proposal numbers are refused before hitting the fixture.
	_, err = pinned.GetProposal(ctx, 0)
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")
	_, err = pinned.CommentProposal(ctx, 1, "   ")
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")
}

// v10OnlyProvider embeds vcs.Provider alone, stripping ProposalReviewer and
// ProposalMerger to prove v1.0 third-party providers degrade cleanly to
// FORGE_UNSUPPORTED even if their Capabilities.Verbs returns vcs.ProposalVerbs.
type v10OnlyProvider struct {
	vcs.Provider
}

func TestV10ProviderWithoutReviewMergeExtensions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Use an unreachable port so any unexpected network attempt would fail
	// with FORGE_UNAVAILABLE instead of FORGE_UNSUPPORTED.
	baseCtor := fakeprovider.New(fakeprovider.Options{
		Verbs:   vcs.ProposalVerbs,
		Labels:  true,
		DialURL: "http://127.0.0.1:1",
	})
	reg := vcs.NewRegistry()
	reg.MustRegister(fakeprovider.Name, func() (vcs.Provider, error) {
		p, err := baseCtor()
		if err != nil {
			return nil, err
		}
		return v10OnlyProvider{Provider: p}, nil
	})

	forge := vcs.Forge{
		Name:     "v10",
		Provider: fakeprovider.Name,
		Cred:     vcs.StaticToken(fakeprovider.ValidToken),
	}
	cat, err := vcs.NewCatalog(reg, []vcs.Forge{forge}, []vcs.RepoConfig{{
		Forge:      "v10",
		Repository: "acme/legacy",
		Role:       vcs.RoleGitOps,
		BaseBranch: "main",
	}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	repo, err := cat.Resolve("acme/legacy")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	caps, err := repo.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.Serves(vcs.VerbProposalReview) {
		t.Errorf("Capabilities.Serves(%s) = true on provider without ProposalReviewer", vcs.VerbProposalReview)
	}
	if caps.Serves(vcs.VerbProposalMerge) {
		t.Errorf("Capabilities.Serves(%s) = true on provider without ProposalMerger", vcs.VerbProposalMerge)
	}
	foundReviewMissing, foundMergeMissing := false, false
	for _, m := range caps.Missing {
		if m == vcs.VerbProposalReview {
			foundReviewMissing = true
		}
		if m == vcs.VerbProposalMerge {
			foundMergeMissing = true
		}
	}
	if !foundReviewMissing || !foundMergeMissing {
		t.Errorf("Capabilities.Missing = %v, want both %s and %s", caps.Missing, vcs.VerbProposalReview, vcs.VerbProposalMerge)
	}

	_, err = repo.ReviewProposal(ctx, 1, vcs.ProposalReviewRequest{
		Decision: vcs.ReviewApprove,
	})
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "v10")

	_, err = repo.MergeProposal(ctx, 1, vcs.ProposalMergeRequest{
		ExpectedRevision: "0123456789abcdef0123456789abcdef01234567",
	})
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "v10")
}

// TestReviewAndMergeProposalGuards verifies every input validation guard on
// Repo.ReviewProposal and Repo.MergeProposal against a live fixture that
// accepts raw requests, ensuring each sub-check fails if commented out.
func TestReviewAndMergeProposalGuards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := fakeprovider.NewFixture(t)

	reg := vcs.NewRegistry()
	reg.MustRegister(fakeprovider.Name, fakeprovider.New(fakeprovider.Options{
		Labels:  true,
		DialURL: fx.Server.URL,
	}))
	forge := vcs.Forge{
		Name:     "f",
		Provider: fakeprovider.Name,
		Cred:     vcs.StaticToken(fakeprovider.ValidToken),
	}
	cat, err := vcs.NewCatalog(reg, []vcs.Forge{forge}, []vcs.RepoConfig{{
		Forge:      "f",
		Repository: "acme/app",
		Role:       vcs.RoleGitOps,
		BaseBranch: "main",
	}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	repo, err := cat.Resolve("acme/app")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	prop, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "agent/feature",
		Title:  "feat: add endpoint",
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	// CodeProposalHeadMoved is non-retryable: caller must re-read first.
	if vcs.CodeProposalHeadMoved.Retryable() {
		t.Error("CodeProposalHeadMoved.Retryable() = true, want false")
	}

	// ReviewProposal: non-positive number.
	_, err = repo.ReviewProposal(ctx, 0, vcs.ProposalReviewRequest{Decision: vcs.ReviewApprove})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")
	_, err = repo.ReviewProposal(ctx, -2, vcs.ProposalReviewRequest{Decision: vcs.ReviewApprove})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")

	// ReviewProposal: invalid decision values.
	for _, badDecision := range []vcs.ReviewDecision{"", "APPROVE", "reject", "lgtm"} {
		_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
			Decision: badDecision,
			Body:     "note",
		})
		vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")
	}

	// ReviewProposal: whitespace-only ExpectedRevision is refused rather than
	// silently dropping the optimistic concurrency check.
	_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: "   \t ",
	})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")

	// ReviewProposal: request_changes and comment require non-empty Body when
	// Comments is empty.
	for _, dec := range []vcs.ReviewDecision{vcs.ReviewRequestChanges, vcs.ReviewComment} {
		_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
			Decision: dec,
			Body:     "   \n\t ",
		})
		vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")
	}

	// ReviewProposal: line comment validation (empty path, non-positive line, empty body).
	badComments := [][]vcs.ReviewLineComment{
		{{Path: "  ", Line: 10, Body: "fix this"}},
		{{Path: "main.go", Line: 0, Body: "fix this"}},
		{{Path: "main.go", Line: -5, Body: "fix this"}},
		{{Path: "main.go", Line: 10, Body: "   "}},
	}
	for _, comments := range badComments {
		_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
			Decision: vcs.ReviewComment,
			Body:     "summary",
			Comments: comments,
		})
		vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")
	}

	// ReviewProposal: comment or request_changes with empty Body succeeds when
	// at least one valid line comment is provided; silent approve succeeds too.
	rvInlineOnly, err := repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision: vcs.ReviewComment,
		Comments: []vcs.ReviewLineComment{{Path: "main.go", Line: 12, Body: "nit: check err"}},
	})
	if err != nil || rvInlineOnly.Decision != vcs.ReviewComment {
		t.Fatalf("ReviewProposal(inline-only comment) = (%+v, %v)", rvInlineOnly, err)
	}
	rvSilentApprove, err := repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: "  " + prop.Revision + "  ",
	})
	if err != nil || rvSilentApprove.Decision != vcs.ReviewApprove {
		t.Fatalf("ReviewProposal(silent approve) = (%+v, %v)", rvSilentApprove, err)
	}

	// MergeProposal: non-positive number.
	_, err = repo.MergeProposal(ctx, 0, vcs.ProposalMergeRequest{ExpectedRevision: prop.Revision})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")

	// MergeProposal: empty or whitespace ExpectedRevision is refused before network.
	for _, emptyRev := range []string{"", "   \t\n"} {
		_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
			ExpectedRevision: emptyRev,
		})
		vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")
	}

	// MergeProposal: invalid MergeMethod is refused before network.
	for _, badMethod := range []vcs.MergeMethod{"fast-forward", "SQUASH", "ff-only"} {
		_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
			ExpectedRevision: prop.Revision,
			Method:           badMethod,
		})
		vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")
	}

	// MergeProposal: valid request merges the proposal.
	merged, err := repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: " " + prop.Revision + " ",
		Method:           vcs.MergeCreateCommit,
	})
	if err != nil || !merged.Merged || merged.Proposal.State != vcs.ProposalMerged {
		t.Fatalf("MergeProposal(valid) = (%+v, %v)", merged, err)
	}
}
