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
	"slices"
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
		vcs.CodeProposalHeadMoved: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(http.StatusConflict, map[string]any{
				"message": "Head branch was modified. Review and try the merge again.",
			}, nil)
			_, err := repo.MergeProposal(context.Background(), 1, vcs.ProposalMergeRequest{
				ExpectedRevision: "0123456789abcdef0123456789abcdef01234567",
			})
			if !strings.Contains(err.Error(), "Head branch was modified") {
				t.Errorf("409 error Detail = %q, want verbatim head-modified message", err.Error())
			}
			return err
		},
		vcs.CodeProtectedBranch: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(http.StatusMethodNotAllowed, map[string]any{
				"message": "At least 1 approving review is required by reviewers with write access.",
			}, nil)
			_, err := repo.MergeProposal(context.Background(), 1, vcs.ProposalMergeRequest{
				ExpectedRevision: "0123456789abcdef0123456789abcdef01234567",
			})
			if !strings.Contains(err.Error(), "At least 1 approving review is required") {
				t.Errorf("405 error Detail = %q, want verbatim branch-protection message", err.Error())
			}
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

func TestReviewAndMergeProposalGitHub(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newTLSFixture(t, true)
	fx.setRequireApproval(true)
	host, port := serverHostPort(t, fx.Server.URL)

	reg := vcs.NewRegistry()
	reg.MustRegister(github.Name, github.New)
	forge := vcs.Forge{
		Name:     "ghes-review",
		Provider: github.Name,
		Host:     host,
		Scheme:   "https",
		Port:     port,
		CAFile:   fx.CAFile,
		Cred:     vcs.StaticToken(validFixtureToken),
	}
	cat, err := vcs.NewCatalog(reg, []vcs.Forge{forge}, []vcs.RepoConfig{{
		Forge:      "ghes-review",
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

	prop, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "agent/fix-slash-1",
		Target: "main",
		Title:  "feat: tighten timeout bounds",
		Body:   "bounds update",
		Draft:  true,
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	oldSHA := prop.Revision

	// 1. Draft PR refusal (405) maps to FORGE_REJECTED, not PROTECTED_BRANCH.
	_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: oldSHA,
		Method:           vcs.MergeSquash,
	})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "ghes-review")
	if !strings.Contains(err.Error(), "still a draft") {
		t.Errorf("draft merge refusal = %q, want verbatim 'still a draft'", err.Error())
	}

	// Mark ready for review.
	draft := false
	if _, err := repo.UpdateProposal(ctx, prop.Number, vcs.ProposalUpdate{Draft: &draft}); err != nil {
		t.Fatalf("UpdateProposal(Draft=false): %v", err)
	}

	// 2. Branch protection requiring approval (405) maps to PROTECTED_BRANCH.
	_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: oldSHA,
		Method:           vcs.MergeSquash,
	})
	vcstest.RequireError(t, err, vcs.CodeProtectedBranch, "ghes-review")
	if !strings.Contains(err.Error(), "approving review is required") {
		t.Errorf("unapproved merge refusal = %q, want verbatim approval message", err.Error())
	}

	// 3. GitHub requires a non-empty summary body on COMMENT and REQUEST_CHANGES
	// reviews even when inline Comments are supplied; provider refuses before network.
	fx.forceNext(http.StatusInternalServerError, map[string]any{"message": "must not reach network"}, nil)
	_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision: vcs.ReviewComment,
		Comments: []vcs.ReviewLineComment{{
			Path: "pkg/timeout.go",
			Line: 19,
			Body: "verify unit is milliseconds",
		}},
	})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "ghes-review")
	if !fx.hasPendingForced() {
		t.Fatal("empty-body COMMENT review reached network instead of failing pre-network")
	}
	fx.clearForced()

	commentRv, err := repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision: vcs.ReviewComment,
		Body:     "inline notes attached",
		Comments: []vcs.ReviewLineComment{{
			Path: "pkg/timeout.go",
			Line: 19,
			Body: "verify unit is milliseconds",
		}},
	})
	if err != nil {
		t.Fatalf("ReviewProposal(comment): %v", err)
	}
	if commentRv.Decision != vcs.ReviewComment || commentRv.Revision != oldSHA || !commentRv.Author.Bot || commentRv.Author.Login != "reviewer-agent" {
		t.Errorf("ReviewProposal(comment) = %+v, want Decision=comment Revision=%q Author=reviewer-agent(Bot)", commentRv, oldSHA)
	}

	// 4. Advance PR head so oldSHA becomes an ancestor commit in CommitHistory.
	// Real GitHub's POST /reviews accepts ancestor commits with 200 OK; our
	// provider's pre-POST GET head.sha check must catch the drift and return
	// PROPOSAL_HEAD_MOVED.
	midSHA := "1234567890abcdef1234567890abcdef12345678"
	fx.advancePullHead(prop.Number, midSHA)
	_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		Body:             "approving stale ancestor must fail",
		ExpectedRevision: oldSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeProposalHeadMoved, "ghes-review")

	// 5a. Concurrent non-force push landing between pre-POST GET and POST /reviews:
	// POST /reviews returns 200 OK (midSHA is still in CommitHistory), and the
	// post-POST GET catches the head move and names the stale review ID.
	newSHA := "abcdef0123456789abcdef0123456789abcdef01"
	fx.advancePullHeadOnNextReviewPost(prop.Number, newSHA)
	_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewComment,
		Body:             "concurrent push landed mid-flight",
		ExpectedRevision: midSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeProposalHeadMoved, "ghes-review")
	if !strings.Contains(err.Error(), "recorded on stale revision") || !strings.Contains(err.Error(), newSHA) {
		t.Errorf("mid-flight push error Detail = %q, want stale review ID and new head SHA", err.Error())
	}

	// 5b. Concurrent force-push between GET and POST (422 on field commit_id)
	// maps to PROPOSAL_HEAD_MOVED.
	fx.forceNextMethod(http.MethodPost, http.StatusUnprocessableEntity, map[string]any{
		"message": "Validation Failed",
		"errors": []map[string]any{{
			"resource": "PullRequestReview",
			"field":    "commit_id",
			"code":     "invalid",
			"message":  "commit_id is not part of the pull request",
		}},
	})
	_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		Body:             "concurrent force-push",
		ExpectedRevision: newSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeProposalHeadMoved, "ghes-review")

	// 6. Self-approval 422 (and 422 mentioning "mismatch" unrelated to commit_id/sha)
	// maps to FORGE_REJECTED with verbatim message (not PROPOSAL_HEAD_MOVED).
	fx.forceNextMethod(http.MethodPost, http.StatusUnprocessableEntity, map[string]any{
		"message": "Unprocessable Entity",
		"errors": []map[string]any{{
			"message": "Review Can not approve your own pull request (author mismatch)",
		}},
	})
	_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: newSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "ghes-review")
	if !strings.Contains(err.Error(), "Can not approve your own pull request") {
		t.Errorf("self-approve 422 Detail = %q, want verbatim message", err.Error())
	}

	// 7. Approving review at current head succeeds.
	approvedRv, err := repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		Body:             "lgtm",
		ExpectedRevision: newSHA,
	})
	if err != nil {
		t.Fatalf("ReviewProposal(approve): %v", err)
	}
	if approvedRv.Decision != vcs.ReviewApprove || approvedRv.Revision != newSHA {
		t.Errorf("ReviewProposal(approve) = %+v, want approve at %q", approvedRv, newSHA)
	}

	// 8a. 405 "Base branch was modified" is transient and maps to FORGE_UNAVAILABLE (retryable).
	fx.forceNext(http.StatusMethodNotAllowed, map[string]any{
		"message": "Base branch was modified. Review and try the merge again.",
	}, nil)
	_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: newSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeForgeUnavailable, "ghes-review")
	if !vcs.CodeOf(err).Retryable() {
		t.Errorf("405 Base branch modified code %s Retryable() = false, want true", vcs.CodeOf(err))
	}

	// 8b. 409 merge conflict must map to FORGE_REJECTED, NOT PROPOSAL_HEAD_MOVED.
	fx.forceNext(http.StatusConflict, map[string]any{
		"message": "Merge conflict",
	}, nil)
	_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: newSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "ghes-review")

	// 9. 405 status-check block, 422 ruleset block, and generic 405 with
	// mergeable_state=blocked map to PROTECTED_BRANCH; generic 405 with
	// mergeable_state=dirty maps to FORGE_REJECTED.
	fx.forceNext(http.StatusMethodNotAllowed, map[string]any{
		"message": "Required status check \"ci/unit\" is expected.",
	}, nil)
	_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: newSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeProtectedBranch, "ghes-review")

	fx.forceNext(http.StatusUnprocessableEntity, map[string]any{
		"message": "Repository rule violations found",
	}, nil)
	_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: newSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeProtectedBranch, "ghes-review")

	fx.setPullMergeableState(prop.Number, "blocked")
	fx.forceNext(http.StatusMethodNotAllowed, map[string]any{
		"message": "Pull Request is not mergeable",
	}, nil)
	_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: newSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeProtectedBranch, "ghes-review")
	if !strings.Contains(err.Error(), "mergeable_state=blocked") {
		t.Errorf("blocked 405 Detail = %q, want mergeable_state=blocked", err.Error())
	}

	fx.setPullMergeableState(prop.Number, "dirty")
	fx.forceNext(http.StatusMethodNotAllowed, map[string]any{
		"message": "Pull Request is not mergeable",
	}, nil)
	_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: newSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "ghes-review")
	fx.setPullMergeableState(prop.Number, "")

	// 10. Merge with DeleteSourceBranch=true deletes slash branch agent/fix-slash-1
	// using unencoded slash segments in /git/refs/heads/agent/fix-slash-1.
	merged, err := repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision:   newSHA,
		Method:             vcs.MergeSquash,
		CommitTitle:        "feat: tighten timeout bounds (#1)",
		CommitMessage:      "Approved by reviewer-agent.",
		DeleteSourceBranch: true,
	})
	if err != nil {
		t.Fatalf("MergeProposal: %v", err)
	}
	if !merged.Merged || merged.Method != vcs.MergeSquash || merged.Revision == "" || merged.Proposal.State != vcs.ProposalMerged {
		t.Errorf("MergeProposal = %+v", merged)
	}
	if !slices.Contains(fx.deletedBranchList(), "agent/fix-slash-1") {
		t.Errorf("deletedBranches = %v, want agent/fix-slash-1", fx.deletedBranchList())
	}

	// 11. Best-effort DeleteSourceBranch when branch was already auto-deleted on
	// the remote (422 Reference does not exist) must not fail the completed merge.
	prop2, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "agent/already-gone",
		Target: "main",
		Title:  "fix: auto-deleted ref",
	})
	if err != nil {
		t.Fatalf("CreateProposal(prop2): %v", err)
	}
	if _, err := repo.ReviewProposal(ctx, prop2.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: prop2.Revision,
	}); err != nil {
		t.Fatalf("ReviewProposal(prop2): %v", err)
	}
	fx.deleteBranchKnown("agent/already-gone")
	if _, err := repo.MergeProposal(ctx, prop2.Number, vcs.ProposalMergeRequest{
		ExpectedRevision:   prop2.Revision,
		Method:             vcs.MergeCreateCommit,
		DeleteSourceBranch: true,
	}); err != nil {
		t.Fatalf("MergeProposal(already-gone branch) failed despite completed merge: %v", err)
	}

	// 12. Cross-repository fork PR with DeleteSourceBranch=true must not delete
	// a same-named branch on the upstream base repository.
	forkNum := fx.seedForkPull("fork-owner/infra", "agent/fix-1", "main", "fix: from fork")
	propFork, err := repo.GetProposal(ctx, forkNum)
	if err != nil {
		t.Fatalf("GetProposal(propFork): %v", err)
	}
	if _, err := repo.ReviewProposal(ctx, propFork.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: propFork.Revision,
	}); err != nil {
		t.Fatalf("ReviewProposal(propFork): %v", err)
	}
	if _, err := repo.MergeProposal(ctx, propFork.Number, vcs.ProposalMergeRequest{
		ExpectedRevision:   propFork.Revision,
		DeleteSourceBranch: true,
	}); err != nil {
		t.Fatalf("MergeProposal(propFork): %v", err)
	}
	if slices.Contains(fx.deletedBranchList(), "agent/fix-1") {
		t.Errorf("cross-fork MergeProposal deleted upstream branch agent/fix-1: %v", fx.deletedBranchList())
	}

	// 13. Deleted-fork PR (head.repo == null) with DeleteSourceBranch=true must
	// NOT treat null head.repo as same-repo and delete upstream agent/fix-1.
	delForkNum := fx.seedDeletedForkPull("agent/fix-1", "main", "fix: from deleted fork")
	propDelFork, err := repo.GetProposal(ctx, delForkNum)
	if err != nil {
		t.Fatalf("GetProposal(propDelFork): %v", err)
	}
	if _, err := repo.ReviewProposal(ctx, propDelFork.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: propDelFork.Revision,
	}); err != nil {
		t.Fatalf("ReviewProposal(propDelFork): %v", err)
	}
	if _, err := repo.MergeProposal(ctx, propDelFork.Number, vcs.ProposalMergeRequest{
		ExpectedRevision:   propDelFork.Revision,
		DeleteSourceBranch: true,
	}); err != nil {
		t.Fatalf("MergeProposal(propDelFork): %v", err)
	}
	if slices.Contains(fx.deletedBranchList(), "agent/fix-1") {
		t.Fatalf("deleted-fork (head.repo=null) MergeProposal deleted upstream branch agent/fix-1: %v", fx.deletedBranchList())
	}

	// 14. DeleteSourceBranch=true must refuse to delete the repository's base
	// branch (e.g. main -> release-1, even if main is unprotected) and must
	// refuse to delete any protected branch (e.g. release/v1 -> main).
	fx.setProtectedBranch("main", false)
	propBaseSource, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "main",
		Target: "release-1",
		Title:  "chore: backport main to release-1",
	})
	if err != nil {
		t.Fatalf("CreateProposal(main -> release-1): %v", err)
	}
	if _, err := repo.ReviewProposal(ctx, propBaseSource.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: propBaseSource.Revision,
	}); err != nil {
		t.Fatalf("ReviewProposal(main -> release-1): %v", err)
	}
	if _, err := repo.MergeProposal(ctx, propBaseSource.Number, vcs.ProposalMergeRequest{
		ExpectedRevision:   propBaseSource.Revision,
		DeleteSourceBranch: true,
	}); err != nil {
		t.Fatalf("MergeProposal(main -> release-1): %v", err)
	}
	if slices.Contains(fx.deletedBranchList(), "main") {
		t.Fatalf("MergeProposal deleted repository base branch main: %v", fx.deletedBranchList())
	}
	fx.setProtectedBranch("main", true)

	fx.setProtectedBranch("release/v1", true)
	propProtSource, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "release/v1",
		Target: "main",
		Title:  "chore: merge release/v1 back to main",
	})
	if err != nil {
		t.Fatalf("CreateProposal(release/v1 -> main): %v", err)
	}
	if _, err := repo.ReviewProposal(ctx, propProtSource.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: propProtSource.Revision,
	}); err != nil {
		t.Fatalf("ReviewProposal(release/v1 -> main): %v", err)
	}
	if _, err := repo.MergeProposal(ctx, propProtSource.Number, vcs.ProposalMergeRequest{
		ExpectedRevision:   propProtSource.Revision,
		DeleteSourceBranch: true,
	}); err != nil {
		t.Fatalf("MergeProposal(release/v1 -> main): %v", err)
	}
	if slices.Contains(fx.deletedBranchList(), "release/v1") {
		t.Fatalf("MergeProposal deleted protected branch release/v1: %v", fx.deletedBranchList())
	}

	// Configured RepoConfig.BaseBranch (e.g. "develop") that differs from the
	// forge's default_branch ("main") and is unprotected must also never be
	// deleted by DeleteSourceBranch=true.
	fx.setProtectedBranch("develop", false)
	devRepo, err := cat.NewRepo(vcs.RepoConfig{
		Forge:      "ghes-review",
		Repository: "acme/infra",
		Role:       vcs.RoleManaged,
		BaseBranch: "develop",
	})
	if err != nil {
		t.Fatalf("NewRepo(BaseBranch=develop): %v", err)
	}
	cfgBaseNum := fx.seedForkPull("acme/infra", "develop", "main", "chore: promote develop to main")
	propCfgBase, err := devRepo.GetProposal(ctx, cfgBaseNum)
	if err != nil {
		t.Fatalf("GetProposal(develop -> main): %v", err)
	}
	if _, err := devRepo.ReviewProposal(ctx, propCfgBase.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: propCfgBase.Revision,
	}); err != nil {
		t.Fatalf("ReviewProposal(develop -> main): %v", err)
	}
	if _, err := devRepo.MergeProposal(ctx, propCfgBase.Number, vcs.ProposalMergeRequest{
		ExpectedRevision:   propCfgBase.Revision,
		DeleteSourceBranch: true,
	}); err != nil {
		t.Fatalf("MergeProposal(develop -> main): %v", err)
	}
	if slices.Contains(fx.deletedBranchList(), "develop") {
		t.Fatalf("MergeProposal deleted configured BaseBranch develop: %v", fx.deletedBranchList())
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
