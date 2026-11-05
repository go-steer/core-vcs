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

package github_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-vcs/internal/vcstest"
	"github.com/go-steer/core-vcs/providers/github"
	"github.com/go-steer/core-vcs/vcs"
)

func serverHostPort(tb testing.TB, rawURL string) (string, int) {
	tb.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		tb.Fatalf("url.Parse(%q): %v", rawURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		tb.Fatalf("Atoi(%q): %v", u.Port(), err)
	}
	return u.Hostname(), port
}

func TestContractPublicGitHub(t *testing.T) {
	t.Parallel()
	fx := newTLSFixture(t, false)

	// Prove the client trusts the fixture's TLS cert via the standard
	// system/CAFile seam by running against the TLS server. For the public
	// host case, ValidateForge forbids CAFile on github.com, so we test a
	// plain HTTP frontend proxy for the public-host case and test TLS+CAFile
	// on the GHES case below.
	httpSrv := newHTTPFrontend(t, fx)

	forge := vcs.Forge{
		Name:     "gh-public",
		Provider: github.Name,
		Cred:     vcs.StaticToken(validFixtureToken),
	}
	ctor := github.NewWithOptions(github.Options{
		PublicAPIBaseURL: httpSrv.URL,
	})

	vcstest.Run(t, vcstest.Case{
		Name:        "github.com",
		Forge:       forge,
		Provider:    ctor,
		PrimaryRepo: "acme/infra",
		Accept: map[string]string{
			"acme/infra":                              "github.com/acme/infra",
			"github.com/acme/infra":                   "github.com/acme/infra",
			"www.github.com/acme/infra":               "github.com/acme/infra",
			"https://github.com/acme/infra":           "github.com/acme/infra",
			"https://github.com/acme/infra.git":       "github.com/acme/infra",
			"https://www.github.com/acme/infra.git":   "github.com/acme/infra",
			"git@github.com:acme/infra.git":           "github.com/acme/infra",
			"ssh://git@ssh.github.com/acme/infra.git": "github.com/acme/infra",
		},
		Reject: []string{
			"single",
			"acme/sub/infra",
			"a/b/c/d",
		},
		Noun:    github.ProposalNoun,
		Serving: vcs.ProposalVerbs,
		Labels:  true,
		Induce:  induceMap(fx, forge, ctor, true),
	})
}

func TestContractEnterpriseServer(t *testing.T) {
	t.Parallel()
	fx := newTLSFixture(t, true)
	host, port := serverHostPort(t, fx.Server.URL)
	portStr := strconv.Itoa(port)

	forge := vcs.Forge{
		Name:     "ghes-corp",
		Provider: github.Name,
		Host:     host,
		Scheme:   "https",
		Port:     port,
		CAFile:   fx.CAFile,
		Cred:     vcs.StaticToken(validFixtureToken),
	}

	vcstest.Run(t, vcstest.Case{
		Name:        "ghes-private-ca",
		Forge:       forge,
		Provider:    github.New,
		PrimaryRepo: "acme/infra",
		Accept: map[string]string{
			"acme/infra":                         host + "/acme/infra",
			host + "/acme/infra":                 host + "/acme/infra",
			host + ":" + portStr + "/acme/infra": host + "/acme/infra",
			"https://" + host + ":" + portStr + "/acme/infra.git": host + "/acme/infra",
			"git@" + host + ":acme/infra.git":                     host + "/acme/infra",
		},
		Reject: []string{
			"single",
			"acme/sub/infra",
		},
		Noun:    github.ProposalNoun,
		Serving: vcs.ProposalVerbs,
		Labels:  true,
		Induce:  induceMap(fx, forge, github.New, false),
	})
}

func induceMap(fx *fixture, forge vcs.Forge, ctor vcs.Constructor, isPublic bool) map[vcs.Code]func(t *testing.T, repo vcs.Repo) error {
	m := map[vcs.Code]func(t *testing.T, repo vcs.Repo) error{
		vcs.CodeForgeUnauthorized: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(http.StatusUnauthorized, map[string]any{"message": "Bad credentials"}, nil)
			_, err := repo.GetProposal(context.Background(), 1)
			return err
		},
		vcs.CodeForgeUnavailable: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(
				http.StatusForbidden,
				map[string]any{"message": "API rate limit exceeded"},
				map[string]string{
					"X-RateLimit-Remaining": "0",
					"X-RateLimit-Reset":     "1793800000",
				},
			)
			_, err := repo.GetProposal(context.Background(), 1)
			if !strings.Contains(err.Error(), "1793800000") {
				t.Errorf("rate-limit error Detail = %q, want reset timestamp", err.Error())
			}
			return err
		},
		vcs.CodeBaseBranchMissing: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(http.StatusNotFound, map[string]any{"message": "Not Found"}, nil)
			_, err := repo.BaseBranchName(context.Background())
			return err
		},
		vcs.CodeForgeUnsupported: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(http.StatusNotFound, map[string]any{"message": "Not Found"}, nil)
			_, err := repo.GetProposal(context.Background(), 999)
			return err
		},
		vcs.CodeForgeCallFailed: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(http.StatusUnprocessableEntity, map[string]any{
				"message": "Validation Failed",
				"errors": []map[string]any{{
					"resource": "PullRequest",
					"field":    "head",
					"code":     "invalid",
					"message":  "No commits between main and agent/fix-1",
				}},
			}, nil)
			_, err := repo.CreateProposal(context.Background(), vcs.ProposalRequest{
				Source: "agent/fix-1",
				Title:  "fix: empty",
			})
			if !strings.Contains(err.Error(), "Validation Failed") || !strings.Contains(err.Error(), "No commits between main and agent/fix-1") {
				t.Errorf("422 error Detail = %q, want message and errors[] verbatim", err.Error())
			}
			return err
		},
		vcs.CodeForgeRejected: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(http.StatusBadRequest, map[string]any{"message": " Problems parsing JSON"}, nil)
			_, err := repo.CommentProposal(context.Background(), 1, "hello")
			return err
		},
	}
	if !isPublic {
		m[vcs.CodeForgeTLSUntrusted] = func(t *testing.T, _ vcs.Repo) error {
			reg := vcs.NewRegistry()
			reg.MustRegister(github.Name, ctor)
			untrusted := forge
			untrusted.CAFile = ""
			cat, err := vcs.NewCatalog(reg, []vcs.Forge{untrusted}, []vcs.RepoConfig{{
				Forge:      untrusted.Name,
				Repository: "acme/infra",
				Role:       vcs.RoleGitOps,
			}})
			if err != nil {
				t.Fatalf("NewCatalog: %v", err)
			}
			r, err := cat.Resolve("acme/infra")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			_, err = r.BaseBranchName(context.Background())
			return err
		}
	}
	return m
}

func newHTTPFrontend(tb testing.TB, fx *fixture) *httptest.Server {
	tb.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fx.serve))
	tb.Cleanup(srv.Close)
	return srv
}

func TestAPIBaseURLAndValidateForge(t *testing.T) {
	t.Parallel()
	raw, err := github.New()
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	p := raw.(*github.Provider)

	if got := p.APIBaseURL(vcs.Forge{Name: "pub", Provider: github.Name}); got != "https://api.github.com" {
		t.Errorf("APIBaseURL(public) = %q, want https://api.github.com", got)
	}
	if got := p.APIBaseURL(vcs.Forge{Name: "ghes", Provider: github.Name, Host: "github.corp.example"}); got != "https://github.corp.example/api/v3" {
		t.Errorf("APIBaseURL(ghes) = %q, want https://github.corp.example/api/v3", got)
	}
	if got := p.APIBaseURL(vcs.Forge{Name: "ghes", Provider: github.Name, Host: "github.corp.example", Port: 8443}); got != "https://github.corp.example:8443/api/v3" {
		t.Errorf("APIBaseURL(ghes:8443) = %q, want https://github.corp.example:8443/api/v3", got)
	}

	// CAFile on public github.com is refused.
	vcstest.RequireError(t, p.ValidateForge(vcs.Forge{
		Name:     "pub",
		Provider: github.Name,
		CAFile:   "/etc/ssl/corp.pem",
	}), vcs.CodeConfigInvalid, "pub")

	// ProposalURLTemplate on github is refused.
	vcstest.RequireError(t, p.ValidateForge(vcs.Forge{
		Name:                "pub",
		Provider:            github.Name,
		ProposalURLTemplate: "https://{host}/{path}",
	}), vcs.CodeConfigInvalid, "pub")

	// Conflicting Host port and Port is refused.
	vcstest.RequireError(t, p.ValidateForge(vcs.Forge{
		Name:     "ghes",
		Provider: github.Name,
		Host:     "github.corp.example:8443",
		Port:     9443,
	}), vcs.CodeConfigInvalid, "ghes")
}

func TestLabelsUnbornBranchAndBotNormalisation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newTLSFixture(t, true)
	host, port := serverHostPort(t, fx.Server.URL)

	reg := vcs.NewRegistry()
	reg.MustRegister(github.Name, github.New)
	forge := vcs.Forge{
		Name:     "ghes",
		Provider: github.Name,
		Host:     host,
		Scheme:   "https",
		Port:     port,
		CAFile:   fx.CAFile,
		Cred:     vcs.StaticToken(validFixtureToken),
	}
	cat, err := vcs.NewCatalog(reg, []vcs.Forge{forge}, []vcs.RepoConfig{{
		Forge:      "ghes",
		Repository: "acme/infra",
		Role:       vcs.RoleGitOps,
	}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	repo, err := cat.Resolve("acme/infra")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// ProtectedBranch on main is true; on an unborn branch ("Branch not found") is false.
	if prot, err := repo.ProtectedBranch(ctx, "main"); err != nil || !prot {
		t.Fatalf("ProtectedBranch(main) = (%v, %v), want (true, nil)", prot, err)
	}
	if prot, err := repo.ProtectedBranch(ctx, "agent/new-topic"); err != nil || prot {
		t.Fatalf("ProtectedBranch(agent/new-topic) = (%v, %v), want (false, nil)", prot, err)
	}

	created, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "agent/fix-1",
		Title:  "feat: add autoscaler",
		Body:   "scales replicas",
		Draft:  true,
		Labels: []string{"automated", "infra"},
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if !created.Author.Bot || created.Author.Login != "platform-agent" {
		t.Errorf("Author = %+v, want Login=platform-agent Bot=true ([bot] stripped)", created.Author)
	}
	if len(created.Labels) != 2 {
		t.Errorf("Labels = %v, want [automated infra]", created.Labels)
	}

	draft := false
	updated, err := repo.UpdateProposal(ctx, created.Number, vcs.ProposalUpdate{
		Draft:        &draft,
		AddLabels:    []string{"reviewed"},
		RemoveLabels: []string{"infra"},
	})
	if err != nil {
		t.Fatalf("UpdateProposal: %v", err)
	}
	if updated.Draft {
		t.Error("updated.Draft = true, want false")
	}

	// ProposalComments returns both discussion and inline review comments.
	if _, err := repo.CommentProposal(ctx, created.Number, "ready for review"); err != nil {
		t.Fatalf("CommentProposal: %v", err)
	}
	cms, err := repo.ProposalComments(ctx, created.Number, vcs.Page{Limit: 10})
	if err != nil {
		t.Fatalf("ProposalComments: %v", err)
	}
	if len(cms.Comments) != 3 {
		t.Fatalf("len(Comments) = %d, want 3 (1 review_comment + 1 review + 1 discussion)", len(cms.Comments))
	}
	if cms.Comments[0].Kind != vcs.CommentReviewComment || cms.Comments[0].Path != "deploy/deployment.yaml" || cms.Comments[0].Line != 42 {
		t.Errorf("Comments[0] = %+v, want inline review_comment with original_line=42", cms.Comments[0])
	}
	if cms.Comments[1].Kind != vcs.CommentReview || cms.Comments[1].Body != "overall looks good after bounds check" {
		t.Errorf("Comments[1] = %+v, want review summary comment", cms.Comments[1])
	}
	if cms.Comments[2].Kind != vcs.CommentDiscussion {
		t.Errorf("Comments[2] = %+v, want discussion comment", cms.Comments[2])
	}
}

func TestLiveGitHub(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("CORE_VCS_GITHUB_TOKEN"))
	repoSpec := strings.TrimSpace(os.Getenv("CORE_VCS_GITHUB_REPO"))
	if token == "" || repoSpec == "" {
		t.Skip("skipping live GitHub test: set CORE_VCS_GITHUB_TOKEN and CORE_VCS_GITHUB_REPO to run")
	}

	writeRoundtrip := strings.TrimSpace(os.Getenv("CORE_VCS_GITHUB_WRITE")) == "1"
	role := vcs.RoleContext
	if writeRoundtrip {
		role = vcs.RoleGitOps
	}

	ctx := context.Background()
	reg := vcs.NewRegistry()
	reg.MustRegister(github.Name, github.New)
	forge := vcs.Forge{
		Name:     "live-github",
		Provider: github.Name,
		Cred:     vcs.StaticToken(token),
		CommitIdentity: vcs.Identity{
			Name:  "core-vcs live test",
			Email: "live-test@go-steer.dev",
		},
	}
	cat, err := vcs.NewCatalog(reg, []vcs.Forge{forge}, []vcs.RepoConfig{{
		Forge:      "live-github",
		Repository: repoSpec,
		Role:       role,
	}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	repo, err := cat.Resolve(repoSpec)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", repoSpec, err)
	}
	caps, err := repo.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.BaseBranch == "" || caps.ProposalNoun != github.ProposalNoun {
		t.Errorf("Capabilities = %+v", caps)
	}
	if _, err := repo.ListProposals(ctx, vcs.ProposalFilter{Page: vcs.Page{Limit: 5}}); err != nil {
		t.Fatalf("ListProposals: %v", err)
	}

	cloneDir := filepath.Join(t.TempDir(), "worktree")
	ws, err := vcs.Clone(ctx, repo, caps.BaseBranch, cloneDir, vcs.Policy{})
	if err != nil {
		t.Fatalf("Clone(%q): %v", caps.BaseBranch, err)
	}
	commits, err := ws.Log(3)
	if err != nil || len(commits) == 0 {
		t.Fatalf("Log(3) = (%v, %v), want >= 1 commit", commits, err)
	}

	if !writeRoundtrip {
		return
	}

	// Verify the in-process TARGET_IS_BRANCH guard refuses pushing to the base branch.
	if _, err := ws.Publish(ctx, ws.ClonedBranch()); !vcs.IsCode(err, vcs.CodeTargetIsBranch) {
		t.Fatalf("Publish(base %q) = %v, want %s", ws.ClonedBranch(), err, vcs.CodeTargetIsBranch)
	}

	branchName := fmt.Sprintf("agent/live-roundtrip-%d", time.Now().UnixNano())
	if err := ws.Branch(branchName); err != nil {
		t.Fatalf("Branch(%q): %v", branchName, err)
	}
	smokeFile := filepath.Join(cloneDir, "SMOKE_TEST.md")
	if err := os.WriteFile(smokeFile, []byte("# Live Round-Trip Test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := ws.Status()
	if err != nil || st.Clean {
		t.Fatalf("Status = (%+v, %v), want dirty worktree", st, err)
	}
	committed, err := ws.Commit("test(live): verify pure-Go proposal round-trip", vcs.Identity{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	diffOut, err := ws.Diff(vcs.DiffOptions{AgainstBase: true})
	if err != nil || !strings.Contains(diffOut, "SMOKE_TEST.md") {
		t.Fatalf("Diff(AgainstBase) = (%q, %v), want SMOKE_TEST.md diff", diffOut, err)
	}

	pub, err := ws.Publish(ctx, branchName)
	if err != nil {
		t.Fatalf("Publish(%q): %v", branchName, err)
	}
	if pub.Revision != committed.Revision {
		t.Errorf("Publish.Revision = %q, want %q", pub.Revision, committed.Revision)
	}

	prop, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
		Title:  "test(live): pure-Go core-vcs round-trip (auto-closing)",
		Body:   "Automated live round-trip test from `TestLiveGitHub` (`CORE_VCS_GITHUB_WRITE=1`).",
		Source: branchName,
		Target: caps.BaseBranch,
		Draft:  true,
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	defer func() {
		if closed, err := repo.CloseProposal(ctx, prop.Number); err != nil {
			t.Errorf("CloseProposal(#%d): %v", prop.Number, err)
		} else if closed.State != vcs.ProposalClosed {
			t.Errorf("CloseProposal(#%d).State = %q, want %q", prop.Number, closed.State, vcs.ProposalClosed)
		}
	}()

	cm, err := repo.CommentProposal(ctx, prop.Number, "Automated comment from `TestLiveGitHub`.")
	if err != nil || cm.ID == "" {
		t.Fatalf("CommentProposal(#%d) = (%+v, %v)", prop.Number, cm, err)
	}
	viewed, err := repo.GetProposal(ctx, prop.Number)
	if err != nil || viewed.Revision != committed.Revision {
		t.Fatalf("GetProposal(#%d) = (%+v, %v), want Revision=%q", prop.Number, viewed, err, committed.Revision)
	}
	pCommits, err := repo.ProposalCommits(ctx, prop.Number, vcs.Page{Limit: 10})
	if err != nil || len(pCommits.Commits) != 1 {
		t.Fatalf("ProposalCommits(#%d) = (%+v, %v), want 1 commit", prop.Number, pCommits, err)
	}
	pComments, err := repo.ProposalComments(ctx, prop.Number, vcs.Page{Limit: 10})
	if err != nil || len(pComments.Comments) == 0 {
		t.Fatalf("ProposalComments(#%d) = (%+v, %v), want >= 1 comment", prop.Number, pComments, err)
	}
}
