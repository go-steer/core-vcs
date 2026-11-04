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

// Package vcstest holds the parameterised contract test every provider
// runs before it registers, and the boundary sweep that proves no file
// outside a provider's own directory names its hosts or API paths.
package vcstest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/core-vcs/vcs"
)

// Case describes one provider configuration under test. A provider with a
// public host and a self-hosted form calls Run twice, once for each.
type Case struct {
	// Name labels the subtest ("public-host", "self-hosted-ca", "link-only").
	Name string
	// Forge is the Forge under test.
	Forge vcs.Forge
	// Provider constructs the provider; Forge.Provider must match its Name.
	Provider vcs.Constructor
	// PrimaryRepo is the canonical path the verb round-trip runs against.
	PrimaryRepo string
	// Accept maps input values (bare, host/path, https://, git@host:path)
	// to the canonical "host/path" Resolve must produce.
	Accept map[string]string
	// Reject lists path values the provider's grammar must refuse with
	// REPO_INVALID (after ParseRepoRef has already accepted them).
	Reject []string
	// Noun is the expected Capabilities.ProposalNoun.
	Noun string
	// Serving is the proposal verbs the fixture answers. Every other verb
	// in vcs.ProposalVerbs must appear in Missing and return
	// FORGE_UNSUPPORTED without touching the fixture.
	Serving []vcs.Verb
	// Labels is the expected Capabilities.Labels.
	Labels bool
	// Induce maps each error code the provider can emit to a function that
	// provokes it against repo. Run checks the code, Forge and non-empty
	// Detail on every one.
	Induce map[vcs.Code]func(t *testing.T, repo vcs.Repo) error
}

// Run runs the contract suite for c.
func Run(t *testing.T, c Case) {
	t.Helper()
	t.Run(c.Name, func(t *testing.T) {
		p, err := c.Provider()
		if err != nil {
			t.Fatalf("constructing provider: %v", err)
		}
		if p.Name() != c.Forge.Provider {
			t.Fatalf("provider Name() = %q, Forge.Provider = %q", p.Name(), c.Forge.Provider)
		}
		hosts := p.Hosts(c.Forge)
		if len(hosts) == 0 || hosts[0] == "" {
			t.Fatalf("Hosts(%+v) = %v, want non-empty canonical-first list", c.Forge, hosts)
		}

		reg := vcs.NewRegistry()
		reg.MustRegister(p.Name(), c.Provider)

		repos := buildRepoConfigs(c)
		cat, err := vcs.NewCatalog(reg, []vcs.Forge{c.Forge}, repos)
		if err != nil {
			t.Fatalf("NewCatalog: %v", err)
		}

		t.Run("identity", func(t *testing.T) { testIdentity(t, cat, c) })
		t.Run("capabilities", func(t *testing.T) { testCapabilities(t, cat, c) })
		t.Run("verbs", func(t *testing.T) { testVerbs(t, cat, c) })
		t.Run("missing-verbs", func(t *testing.T) { testMissingVerbs(t, cat, c) })
		t.Run("labels-guard", func(t *testing.T) { testLabelsGuard(t, cat, c) })
		t.Run("errors", func(t *testing.T) { testInducedErrors(t, cat, c) })
	})
}

func buildRepoConfigs(c Case) []vcs.RepoConfig {
	seen := map[string]bool{c.PrimaryRepo: true}
	out := []vcs.RepoConfig{{
		Forge:      c.Forge.Name,
		Repository: c.PrimaryRepo,
		Role:       vcs.RoleGitOps,
	}}
	for _, want := range c.Accept {
		_, path, ok := strings.Cut(want, "/")
		if !ok || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, vcs.RepoConfig{
			Forge:      c.Forge.Name,
			Repository: path,
			Role:       vcs.RoleManaged,
		})
	}
	return out
}

func testIdentity(t *testing.T, cat *vcs.Catalog, c Case) {
	if len(c.Accept) == 0 {
		t.Fatal("Case.Accept is empty; list at least bare, host/path and URL forms")
	}
	for in, want := range c.Accept {
		repo, err := cat.Resolve(in)
		if err != nil {
			t.Errorf("Resolve(%q) unexpected error: %v", in, err)
			continue
		}
		if got := repo.String(); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
		if u := repo.CloneURL(); !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			t.Errorf("CloneURL(%q) = %q, want http(s)://", in, u)
		}
	}
	for _, bad := range c.Reject {
		_, err := cat.NewRepo(vcs.RepoConfig{
			Forge:      c.Forge.Name,
			Repository: bad,
			Role:       vcs.RoleManaged,
		})
		RequireError(t, err, vcs.CodeRepoInvalid, c.Forge.Name)
	}
	// Traversal and leading-dash segments are refused before the provider
	// sees them, regardless of the provider's grammar.
	for _, bad := range []string{"../escape", "acme/../secret", "-flag/repo", "acme/-repo"} {
		_, err := cat.Resolve(bad)
		RequireError(t, err, vcs.CodeRepoInvalid, "")
	}
	// A host not in the catalog is never fallen back from.
	_, err := cat.Resolve("https://unconfigured.invalid/" + c.PrimaryRepo)
	RequireError(t, err, vcs.CodeForgeUnsupported, "")
}

func testCapabilities(t *testing.T, cat *vcs.Catalog, c Case) {
	ctx := context.Background()
	repo, err := cat.Resolve(c.PrimaryRepo)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", c.PrimaryRepo, err)
	}
	caps, err := repo.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.ProposalNoun != c.Noun {
		t.Errorf("ProposalNoun = %q, want %q", caps.ProposalNoun, c.Noun)
	}
	if caps.BaseBranch == "" {
		t.Error("BaseBranch is empty")
	}
	if caps.Labels != c.Labels {
		t.Errorf("Labels = %v, want %v", caps.Labels, c.Labels)
	}
	for _, v := range vcs.ProposalVerbs {
		wantServed := slices.Contains(c.Serving, v)
		if got := caps.Serves(v); got != wantServed {
			t.Errorf("Serves(%s) = %v, want %v", v, got, wantServed)
		}
		if gotMissing := slices.Contains(caps.Missing, v); gotMissing == wantServed {
			t.Errorf("Missing contains %s = %v, want %v", v, gotMissing, !wantServed)
		}
	}
}

func testVerbs(t *testing.T, cat *vcs.Catalog, c Case) {
	ctx := context.Background()
	repo, err := cat.Resolve(c.PrimaryRepo)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", c.PrimaryRepo, err)
	}
	serves := func(v vcs.Verb) bool { return slices.Contains(c.Serving, v) }

	if c.Forge.Cred != nil {
		auth, err := repo.CloneAuth(ctx)
		if err != nil {
			t.Fatalf("CloneAuth: %v", err)
		}
		if auth == nil || auth.Username == "" || auth.Password == "" {
			t.Fatalf("CloneAuth = %+v, want non-empty user and password", auth)
		}
	}
	if _, err := repo.ProtectedBranch(ctx, "main"); err != nil {
		t.Fatalf("ProtectedBranch: %v", err)
	}

	if !serves(vcs.VerbProposalCreate) {
		return
	}
	created, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "agent/fix-1",
		Title:  "fix: align replica count",
		Body:   "diagnosed drift; rollback by reverting",
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if created.URL == "" {
		t.Error("CreateProposal returned empty URL")
	}
	if c.Noun == vcs.ProposalNounLink {
		if created.Number != 0 {
			t.Errorf("link proposal Number = %d, want 0", created.Number)
		}
		return
	}
	if created.Number <= 0 || created.State != vcs.ProposalOpen || created.Target == "" {
		t.Fatalf("CreateProposal = %+v, want positive Number, open State and filled Target", created)
	}

	if serves(vcs.VerbProposalList) {
		list, err := repo.ListProposals(ctx, vcs.ProposalFilter{Source: "agent/fix-1", Page: vcs.Page{Limit: 10}})
		if err != nil {
			t.Fatalf("ListProposals: %v", err)
		}
		if len(list.Proposals) == 0 {
			t.Error("ListProposals returned no proposals after CreateProposal")
		}
	}
	if serves(vcs.VerbProposalView) {
		got, err := repo.GetProposal(ctx, created.Number)
		if err != nil {
			t.Fatalf("GetProposal: %v", err)
		}
		if got.Number != created.Number || got.Title != created.Title {
			t.Errorf("GetProposal = %+v, want Number=%d Title=%q", got, created.Number, created.Title)
		}
	}
	if serves(vcs.VerbProposalComment) {
		cm, err := repo.CommentProposal(ctx, created.Number, "verified in staging")
		if err != nil {
			t.Fatalf("CommentProposal: %v", err)
		}
		if cm.ID == "" || cm.Body != "verified in staging" {
			t.Errorf("CommentProposal = %+v", cm)
		}
	}
	if serves(vcs.VerbProposalComments) {
		cms, err := repo.ProposalComments(ctx, created.Number, vcs.Page{Limit: 10})
		if err != nil {
			t.Fatalf("ProposalComments: %v", err)
		}
		if serves(vcs.VerbProposalComment) && len(cms.Comments) == 0 {
			t.Error("ProposalComments returned none after CommentProposal")
		}
	}
	if serves(vcs.VerbProposalCommits) {
		commits, err := repo.ProposalCommits(ctx, created.Number, vcs.Page{Limit: 10})
		if err != nil {
			t.Fatalf("ProposalCommits: %v", err)
		}
		if len(commits.Commits) == 0 || commits.Commits[0].Revision == "" {
			t.Errorf("ProposalCommits = %+v, want at least one commit with Revision", commits)
		}
	}
	if serves(vcs.VerbProposalUpdate) {
		newTitle := "fix: align replica count (revised)"
		up, err := repo.UpdateProposal(ctx, created.Number, vcs.ProposalUpdate{Title: &newTitle})
		if err != nil {
			t.Fatalf("UpdateProposal: %v", err)
		}
		if up.Title != newTitle {
			t.Errorf("UpdateProposal Title = %q, want %q", up.Title, newTitle)
		}
	}
	if serves(vcs.VerbProposalClose) {
		closed, err := repo.CloseProposal(ctx, created.Number)
		if err != nil {
			t.Fatalf("CloseProposal: %v", err)
		}
		if closed.State != vcs.ProposalClosed {
			t.Errorf("CloseProposal State = %q, want %q", closed.State, vcs.ProposalClosed)
		}
	}
}

func testMissingVerbs(t *testing.T, cat *vcs.Catalog, c Case) {
	ctx := context.Background()
	repo, err := cat.Resolve(c.PrimaryRepo)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, v := range vcs.ProposalVerbs {
		if slices.Contains(c.Serving, v) {
			continue
		}
		err := callVerb(ctx, repo, v)
		RequireError(t, err, vcs.CodeForgeUnsupported, c.Forge.Name)
	}
}

func testLabelsGuard(t *testing.T, cat *vcs.Catalog, c Case) {
	if c.Labels {
		return
	}
	ctx := context.Background()
	repo, err := cat.Resolve(c.PrimaryRepo)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if slices.Contains(c.Serving, vcs.VerbProposalCreate) {
		_, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
			Source: "agent/fix-1",
			Title:  "fix: with label",
			Labels: []string{"automated"},
		})
		RequireError(t, err, vcs.CodeLabelsUnsupported, c.Forge.Name)
	}
	if slices.Contains(c.Serving, vcs.VerbProposalList) {
		_, err := repo.ListProposals(ctx, vcs.ProposalFilter{Labels: []string{"automated"}})
		RequireError(t, err, vcs.CodeLabelsUnsupported, c.Forge.Name)
	}
	if slices.Contains(c.Serving, vcs.VerbProposalUpdate) {
		_, err := repo.UpdateProposal(ctx, 1, vcs.ProposalUpdate{AddLabels: []string{"automated"}})
		RequireError(t, err, vcs.CodeLabelsUnsupported, c.Forge.Name)
		_, err = repo.UpdateProposal(ctx, 1, vcs.ProposalUpdate{RemoveLabels: []string{"automated"}})
		RequireError(t, err, vcs.CodeLabelsUnsupported, c.Forge.Name)
	}
}

func testInducedErrors(t *testing.T, cat *vcs.Catalog, c Case) {
	repo, err := cat.Resolve(c.PrimaryRepo)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for code, fn := range c.Induce {
		t.Run(string(code), func(t *testing.T) {
			err := fn(t, repo)
			RequireError(t, err, code, c.Forge.Name)
		})
	}
}

func callVerb(ctx context.Context, repo vcs.Repo, v vcs.Verb) error {
	switch v {
	case vcs.VerbProposalCreate:
		_, err := repo.CreateProposal(ctx, vcs.ProposalRequest{Source: "agent/x", Title: "t"})
		return err
	case vcs.VerbProposalList:
		_, err := repo.ListProposals(ctx, vcs.ProposalFilter{})
		return err
	case vcs.VerbProposalView:
		_, err := repo.GetProposal(ctx, 1)
		return err
	case vcs.VerbProposalComments:
		_, err := repo.ProposalComments(ctx, 1, vcs.Page{})
		return err
	case vcs.VerbProposalCommits:
		_, err := repo.ProposalCommits(ctx, 1, vcs.Page{})
		return err
	case vcs.VerbProposalComment:
		_, err := repo.CommentProposal(ctx, 1, "note")
		return err
	case vcs.VerbProposalUpdate:
		s := "t"
		_, err := repo.UpdateProposal(ctx, 1, vcs.ProposalUpdate{Title: &s})
		return err
	case vcs.VerbProposalClose:
		_, err := repo.CloseProposal(ctx, 1)
		return err
	default:
		return nil
	}
}

// RequireError asserts that err is a *vcs.Error with wantCode, a
// non-empty Detail, and (when wantForge != "") that Forge.
func RequireError(t testing.TB, err error, wantCode vcs.Code, wantForge string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want code %s", wantCode)
	}
	var ve *vcs.Error
	if !errors.As(err, &ve) {
		t.Fatalf("error %T (%v) is not a *vcs.Error", err, err)
	}
	if !ve.Code.Valid() {
		t.Errorf("Code %q is not in vcs.Codes", ve.Code)
	}
	if ve.Code != wantCode {
		t.Errorf("Code = %s, want %s (err: %v)", ve.Code, wantCode, err)
	}
	if strings.TrimSpace(ve.Detail) == "" {
		t.Errorf("Detail is empty on %+v", ve)
	}
	if wantForge != "" && ve.Forge != wantForge {
		t.Errorf("Forge = %q, want %q (err: %v)", ve.Forge, wantForge, err)
	}
}
