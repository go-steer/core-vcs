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
