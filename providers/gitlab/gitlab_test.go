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

package gitlab_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/go-steer/core-vcs/internal/vcstest"
	"github.com/go-steer/core-vcs/providers/gitlab"
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

func TestContractPublicGitLab(t *testing.T) {
	t.Parallel()
	fx := newTLSFixture(t)
	httpSrv := httptest.NewServer(http.HandlerFunc(fx.serve))
	t.Cleanup(httpSrv.Close)

	forge := vcs.Forge{
		Name:     "gl-public",
		Provider: gitlab.Name,
		Cred:     vcs.StaticToken(validFixtureToken),
	}
	ctor := gitlab.NewWithOptions(gitlab.Options{
		PublicAPIBaseURL: httpSrv.URL + gitlab.APIPrefix,
	})

	vcstest.Run(t, vcstest.Case{
		Name:        "gitlab.com",
		Forge:       forge,
		Provider:    ctor,
		PrimaryRepo: "acme/platform/infra",
		Accept: map[string]string{
			"acme/infra":                                 "gitlab.com/acme/infra",
			"acme/platform/infra":                        "gitlab.com/acme/platform/infra",
			"acme/platform/deep/nested/infra":            "gitlab.com/acme/platform/deep/nested/infra",
			"gitlab.com/acme/platform/infra":             "gitlab.com/acme/platform/infra",
			"www.gitlab.com/acme/platform/infra":         "gitlab.com/acme/platform/infra",
			"https://gitlab.com/acme/platform/infra.git": "gitlab.com/acme/platform/infra",
			"git@gitlab.com:acme/platform/infra.git":     "gitlab.com/acme/platform/infra",
		},
		Reject: []string{
			"single-segment",
		},
		Noun:    gitlab.ProposalNoun,
		Serving: vcs.ProposalVerbs,
		Labels:  true,
		Induce:  induceMap(fx, forge, ctor, true),
	})
}

func TestContractSelfManagedPrivateCA(t *testing.T) {
	t.Parallel()
	fx := newTLSFixture(t)
	host, port := serverHostPort(t, fx.Server.URL)
	portStr := strconv.Itoa(port)

	forge := vcs.Forge{
		Name:     "gl-self",
		Provider: gitlab.Name,
		Host:     host,
		Scheme:   "https",
		Port:     port,
		CAFile:   fx.CAFile,
		Cred:     vcs.StaticToken(validFixtureToken),
	}

	vcstest.Run(t, vcstest.Case{
		Name:        "gitlab-self-managed-private-ca",
		Forge:       forge,
		Provider:    gitlab.New,
		PrimaryRepo: "acme/platform/infra",
		Accept: map[string]string{
			"acme/platform/infra":                         host + "/acme/platform/infra",
			"acme/infra":                                  host + "/acme/infra",
			host + "/acme/platform/infra":                 host + "/acme/platform/infra",
			host + ":" + portStr + "/acme/platform/infra": host + "/acme/platform/infra",
			"https://" + host + ":" + portStr + "/acme/platform/infra.git": host + "/acme/platform/infra",
			"git@" + host + ":acme/platform/infra.git":                     host + "/acme/platform/infra",
		},
		Reject: []string{
			"single-segment",
		},
		Noun:    gitlab.ProposalNoun,
		Serving: vcs.ProposalVerbs,
		Labels:  true,
		Induce:  induceMap(fx, forge, gitlab.New, false),
	})
}

func induceMap(fx *fixture, forge vcs.Forge, ctor vcs.Constructor, isPublic bool) map[vcs.Code]func(t *testing.T, repo vcs.Repo) error {
	m := map[vcs.Code]func(t *testing.T, repo vcs.Repo) error{
		vcs.CodeForgeUnauthorized: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(http.StatusUnauthorized, map[string]any{"message": "401 Unauthorized"}, nil)
			_, err := repo.GetProposal(context.Background(), 1)
			return err
		},
		vcs.CodeForgeUnavailable: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(
				http.StatusTooManyRequests,
				map[string]any{"message": "Retry later"},
				map[string]string{"RateLimit-Reset": "1793800000"},
			)
			_, err := repo.GetProposal(context.Background(), 1)
			if !strings.Contains(err.Error(), "1793800000") {
				t.Errorf("rate-limit Detail = %q, want reset timestamp", err.Error())
			}
			return err
		},
		vcs.CodeBaseBranchMissing: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(http.StatusNotFound, map[string]any{"message": "404 Project Not Found"}, nil)
			_, err := repo.BaseBranchName(context.Background())
			return err
		},
		vcs.CodeForgeUnsupported: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(http.StatusNotFound, map[string]any{"message": "404 Not found"}, nil)
			_, err := repo.GetProposal(context.Background(), 999)
			return err
		},
		vcs.CodeForgeCallFailed: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(http.StatusUnprocessableEntity, map[string]any{
				"message": map[string]any{
					"validate_branches": []string{"Cannot Create: This merge request already exists"},
				},
			}, nil)
			_, err := repo.CreateProposal(context.Background(), vcs.ProposalRequest{
				Source: "agent/fix-1",
				Title:  "fix: duplicate",
			})
			if !strings.Contains(err.Error(), "already exists") {
				t.Errorf("422 Detail = %q, want structured message verbatim", err.Error())
			}
			return err
		},
		vcs.CodeForgeRejected: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(http.StatusBadRequest, map[string]any{"error": "title is missing"}, nil)
			_, err := repo.CommentProposal(context.Background(), 1, "note")
			return err
		},
		vcs.CodeProposalHeadMoved: func(t *testing.T, repo vcs.Repo) error {
			fx.forceNext(http.StatusConflict, map[string]any{
				"message": "SHA does not match HEAD of source branch: fedcba9876543210fedcba9876543210fedcba98",
			}, nil)
			_, err := repo.MergeProposal(context.Background(), 1, vcs.ProposalMergeRequest{
				ExpectedRevision: "0123456789abcdef0123456789abcdef01234567",
			})
			if !strings.Contains(err.Error(), "SHA does not match HEAD") {
				t.Errorf("409 Detail = %q, want verbatim SHA mismatch message", err.Error())
			}
			return err
		},
		vcs.CodeProtectedBranch: func(t *testing.T, repo vcs.Repo) error {
			openMR, err := repo.CreateProposal(context.Background(), vcs.ProposalRequest{
				Source: "agent/unapproved",
				Title:  "fix: requires approval",
			})
			if err != nil {
				t.Fatalf("CreateProposal: %v", err)
			}
			fx.setRequireApproval(true)
			defer fx.setRequireApproval(false)
			_, err = repo.MergeProposal(context.Background(), openMR.Number, vcs.ProposalMergeRequest{
				ExpectedRevision: openMR.Revision,
			})
			if !strings.Contains(err.Error(), "405 Method Not Allowed") || !strings.Contains(err.Error(), "detailed_merge_status=not_approved") {
				t.Errorf("405 Detail = %q, want generic 405 body and detailed_merge_status=not_approved", err.Error())
			}
			return err
		},
	}
	if !isPublic {
		m[vcs.CodeForgeTLSUntrusted] = func(t *testing.T, _ vcs.Repo) error {
			reg := vcs.NewRegistry()
			reg.MustRegister(gitlab.Name, ctor)
			untrusted := forge
			untrusted.CAFile = ""
			cat, err := vcs.NewCatalog(reg, []vcs.Forge{untrusted}, []vcs.RepoConfig{{
				Forge:      untrusted.Name,
				Repository: "acme/platform/infra",
				Role:       vcs.RoleGitOps,
			}})
			if err != nil {
				t.Fatalf("NewCatalog: %v", err)
			}
			r, err := cat.Resolve("acme/platform/infra")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			_, err = r.BaseBranchName(context.Background())
			return err
		}
	}
	return m
}

func TestValidateForgeAndSelfManagedHTTP(t *testing.T) {
	t.Parallel()
	raw, err := gitlab.New()
	if err != nil {
		t.Fatalf("gitlab.New: %v", err)
	}
	p := raw.(*gitlab.Provider)

	if got := p.APIBaseURL(vcs.Forge{Name: "pub", Provider: gitlab.Name}); got != "https://gitlab.com/api/v4" {
		t.Errorf("APIBaseURL(public) = %q, want https://gitlab.com/api/v4", got)
	}
	if got := p.APIBaseURL(vcs.Forge{Name: "incluster", Provider: gitlab.Name, Host: "gitlab.svc.cluster.local", Scheme: "http", Port: 8080}); got != "http://gitlab.svc.cluster.local:8080/api/v4" {
		t.Errorf("APIBaseURL(incluster) = %q, want http://gitlab.svc.cluster.local:8080/api/v4", got)
	}

	// Scheme: http is allowed for self-managed GitLab, refused on public gitlab.com.
	if err := p.ValidateForge(vcs.Forge{Name: "incluster", Provider: gitlab.Name, Host: "gitlab.svc.cluster.local", Scheme: "http"}); err != nil {
		t.Errorf("ValidateForge(self-managed http) = %v, want nil", err)
	}
	vcstest.RequireError(t, p.ValidateForge(vcs.Forge{Name: "pub", Provider: gitlab.Name, Scheme: "http"}), vcs.CodeConfigInvalid, "pub")
	vcstest.RequireError(t, p.ValidateForge(vcs.Forge{Name: "pub", Provider: gitlab.Name, CAFile: "/etc/ssl/ca.pem"}), vcs.CodeConfigInvalid, "pub")
	vcstest.RequireError(t, p.ValidateForge(vcs.Forge{Name: "pub", Provider: gitlab.Name, ProposalURLTemplate: "https://{host}/{path}"}), vcs.CodeConfigInvalid, "pub")
}

func TestProtectedBranchesDraftLabelsAndNotes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newTLSFixture(t)
	host, port := serverHostPort(t, fx.Server.URL)

	reg := vcs.NewRegistry()
	reg.MustRegister(gitlab.Name, gitlab.New)
	forge := vcs.Forge{
		Name:     "gl",
		Provider: gitlab.Name,
		Host:     host,
		Scheme:   "https",
		Port:     port,
		CAFile:   fx.CAFile,
		Cred:     vcs.StaticToken(validFixtureToken),
	}
	cat, err := vcs.NewCatalog(reg, []vcs.Forge{forge}, []vcs.RepoConfig{{
		Forge:      "gl",
		Repository: "acme/platform/infra",
		Role:       vcs.RoleGitOps,
	}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	repo, err := cat.Resolve("acme/platform/infra")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Exact and wildcard protected branch rules match; agent branch does not.
	if prot, err := repo.ProtectedBranch(ctx, "main"); err != nil || !prot {
		t.Fatalf("ProtectedBranch(main) = (%v, %v), want (true, nil)", prot, err)
	}
	if prot, err := repo.ProtectedBranch(ctx, "release/v1"); err != nil || !prot {
		t.Fatalf("ProtectedBranch(release/v1) = (%v, %v), want (true, nil)", prot, err)
	}
	if prot, err := repo.ProtectedBranch(ctx, "release/v1/hotfix"); err != nil || !prot {
		t.Fatalf("ProtectedBranch(release/v1/hotfix) = (%v, %v), want (true, nil)", prot, err)
	}
	if prot, err := repo.ProtectedBranch(ctx, "agent/fix-1"); err != nil || prot {
		t.Fatalf("ProtectedBranch(agent/fix-1) = (%v, %v), want (false, nil)", prot, err)
	}

	created, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "agent/fix-1",
		Title:  "feat: tune HPA",
		Body:   "adjust minReplicas",
		Draft:  true,
		Labels: []string{"automated", "sre"},
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if !created.Draft || created.Title != "feat: tune HPA" {
		t.Errorf("CreateProposal = %+v, want Draft=true and stripped Title=\"feat: tune HPA\"", created)
	}

	draft := false
	updated, err := repo.UpdateProposal(ctx, created.Number, vcs.ProposalUpdate{
		Draft:        &draft,
		AddLabels:    []string{"ready"},
		RemoveLabels: []string{"sre"},
	})
	if err != nil {
		t.Fatalf("UpdateProposal: %v", err)
	}
	if updated.Draft {
		t.Error("updated.Draft = true, want false")
	}

	// System notes are skipped; positioned notes become review_comment; discussion notes become discussion.
	if _, err := repo.CommentProposal(ctx, created.Number, "ready for merge"); err != nil {
		t.Fatalf("CommentProposal: %v", err)
	}
	cms, err := repo.ProposalComments(ctx, created.Number, vcs.Page{Limit: 10})
	if err != nil {
		t.Fatalf("ProposalComments: %v", err)
	}
	if len(cms.Comments) != 2 {
		t.Fatalf("len(Comments) = %d, want 2 (system note filtered out)", len(cms.Comments))
	}
	if cms.Comments[0].Kind != vcs.CommentReviewComment || cms.Comments[0].Path != "charts/values.yaml" || cms.Comments[0].Line != 27 {
		t.Errorf("Comments[0] = %+v, want review_comment on charts/values.yaml:27", cms.Comments[0])
	}
	if cms.Comments[1].Kind != vcs.CommentDiscussion {
		t.Errorf("Comments[1] = %+v, want discussion comment", cms.Comments[1])
	}
}

func TestReviewAndMergeProposalGitLab(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newTLSFixture(t)
	fx.setRequireApproval(true)
	host, port := serverHostPort(t, fx.Server.URL)

	reg := vcs.NewRegistry()
	reg.MustRegister(gitlab.Name, gitlab.New)
	forge := vcs.Forge{
		Name:     "gl-review",
		Provider: gitlab.Name,
		Host:     host,
		Scheme:   "https",
		Port:     port,
		CAFile:   fx.CAFile,
		Cred:     vcs.StaticToken(validFixtureToken),
	}
	cat, err := vcs.NewCatalog(reg, []vcs.Forge{forge}, []vcs.RepoConfig{{
		Forge:      "gl-review",
		Repository: "acme/platform/deep/infra",
		Role:       vcs.RoleGitOps,
	}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	repo, err := cat.Resolve("acme/platform/deep/infra")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	prop, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "agent/fix-limits",
		Target: "main",
		Title:  "feat: raise memory limit",
		Body:   "avoids OOMKill",
		Draft:  true,
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	initialSHA := prop.Revision

	// 1. MergeRebase is refused with FORGE_UNSUPPORTED before any network call.
	fx.forceNext(http.StatusInternalServerError, map[string]any{"message": "must not reach network"}, nil)
	_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: initialSHA,
		Method:           vcs.MergeRebase,
	})
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "gl-review")
	if !fx.hasPendingForced() {
		t.Fatal("MergeRebase guard failed to stop before network: forced 500 was consumed")
	}
	fx.clearForced()

	// 2. Draft MR refusal: real GitLab PUT /merge returns generic "405 Method Not Allowed",
	// and GET /merge_requests/{iid} reports detailed_merge_status="draft_status" -> FORGE_REJECTED.
	_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: initialSHA,
		Method:           vcs.MergeSquash,
	})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "gl-review")
	if !strings.Contains(err.Error(), "405 Method Not Allowed") || !strings.Contains(err.Error(), "detailed_merge_status=draft_status") {
		t.Errorf("draft merge refusal = %q, want generic 405 body and detailed_merge_status=draft_status", err.Error())
	}

	// Mark ready for review.
	draft := false
	if _, err := repo.UpdateProposal(ctx, prop.Number, vcs.ProposalUpdate{Draft: &draft}); err != nil {
		t.Fatalf("UpdateProposal(Draft=false): %v", err)
	}

	// 3. Policy/check detailed_merge_status values behind generic 405 responses map to PROTECTED_BRANCH;
	// structural/conflict values map to FORGE_REJECTED.
	_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: initialSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeProtectedBranch, "gl-review")
	if !strings.Contains(err.Error(), "405 Method Not Allowed") || !strings.Contains(err.Error(), "detailed_merge_status=not_approved") {
		t.Errorf("not_approved refusal = %q, want generic 405 body and detailed_merge_status=not_approved", err.Error())
	}

	for _, dms := range []string{
		"requested_changes",
		"ci_must_pass",
		"ci_still_running",
		"discussions_not_resolved",
		"external_status_checks",
		"blocked_status",
		"security_policy_violations",
	} {
		fx.setMRDetailedMergeStatus(prop.Number, dms)
		_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
			ExpectedRevision: initialSHA,
		})
		vcstest.RequireError(t, err, vcs.CodeProtectedBranch, "gl-review")
		if !strings.Contains(err.Error(), "detailed_merge_status="+dms) {
			t.Errorf("%s Detail = %q, want detailed_merge_status=%s", dms, err.Error(), dms)
		}
	}

	for _, tc := range []struct {
		dms        string
		wantSubstr string
	}{
		{dms: "conflict", wantSubstr: "Branch cannot be merged"},
		{dms: "need_rebase", wantSubstr: "405 Method Not Allowed"},
		{dms: "broken_status", wantSubstr: "405 Method Not Allowed"},
	} {
		fx.setMRDetailedMergeStatus(prop.Number, tc.dms)
		_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
			ExpectedRevision: initialSHA,
		})
		vcstest.RequireError(t, err, vcs.CodeForgeRejected, "gl-review")
		if !strings.Contains(err.Error(), tc.wantSubstr) || !strings.Contains(err.Error(), "detailed_merge_status="+tc.dms) {
			t.Errorf("%s Detail = %q, want %q and detailed_merge_status=%s", tc.dms, err.Error(), tc.wantSubstr, tc.dms)
		}
	}
	fx.setMRDetailedMergeStatus(prop.Number, "")

	// 4. Missing diff_refs when inline Comments are requested returns FORGE_REJECTED
	// without approving the MR.
	fx.setMRHideDiffRefs(prop.Number, true)
	_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: initialSHA,
		Comments: []vcs.ReviewLineComment{{
			Path: "charts/values.yaml",
			Line: 12,
			Body: "bump memory limit",
		}},
	})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "gl-review")
	if fx.snapshotMR(prop.Number).Approved {
		t.Fatal("MR became approved despite missing diff_refs failure")
	}
	fx.setMRHideDiffRefs(prop.Number, false)

	// 5. Validation error on inline /discussions must abort BEFORE /approve is called.
	fx.forceNextOnSuffix("/discussions", http.StatusBadRequest, map[string]any{
		"message": "line_code is out of diff range",
	})
	_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: initialSHA,
		Comments: []vcs.ReviewLineComment{{
			Path: "charts/values.yaml",
			Line: 999,
			Body: "bad line",
		}},
	})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "gl-review")
	if fx.snapshotMR(prop.Number).Approved {
		t.Fatal("MR became approved despite /discussions failure")
	}

	// 6. diff_refs.head_sha mismatch returns PROPOSAL_HEAD_MOVED.
	fx.setMRHeads(prop.Number, initialSHA, "2222222222222222222222222222222222222222")
	_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: initialSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeProposalHeadMoved, "gl-review")
	fx.setMRHeads(prop.Number, initialSHA, initialSHA)

	// 7. Concurrent 409 on POST /approve maps to PROPOSAL_HEAD_MOVED.
	fx.forceNextOnSuffix("/approve", http.StatusConflict, map[string]any{
		"message": "SHA does not match HEAD of source branch",
	})
	_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: initialSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeProposalHeadMoved, "gl-review")

	// 7a. Concurrent push arriving during non-approve review submission (/notes
	// or /discussions) is caught by the post-submit head re-check and returns
	// PROPOSAL_HEAD_MOVED.
	concurrentSHA := "3333333333333333333333333333333333333333"
	fx.advanceMRHeadOnNextNotePost(prop.Number, concurrentSHA)
	_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewComment,
		Body:             "race check on summary note",
		ExpectedRevision: initialSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeProposalHeadMoved, "gl-review")
	if !strings.Contains(err.Error(), concurrentSHA) {
		t.Errorf("concurrent push Detail = %q, want new head %s", err.Error(), concurrentSHA)
	}
	fx.setMRHeads(prop.Number, initialSHA, initialSHA)

	fx.advanceMRHeadOnNextNotePost(prop.Number, concurrentSHA)
	_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewRequestChanges,
		ExpectedRevision: initialSHA,
		Comments: []vcs.ReviewLineComment{{
			Path: "charts/values.yaml",
			Line: 10,
			Body: "race check on inline discussion",
		}},
	})
	vcstest.RequireError(t, err, vcs.CodeProposalHeadMoved, "gl-review")
	fx.setMRHeads(prop.Number, initialSHA, initialSHA)

	// 8. Inline-only ReviewComment (no summary Body) anchors on diff_refs and populates ProposalReview.
	inlineRv, err := repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision: vcs.ReviewComment,
		Comments: []vcs.ReviewLineComment{{
			Path: "charts/values.yaml",
			Line: 14,
			Body: "confirm memory request <= limit",
		}},
	})
	if err != nil {
		t.Fatalf("ReviewProposal(inline-only comment): %v", err)
	}
	if inlineRv.ID == "" || inlineRv.Decision != vcs.ReviewComment || inlineRv.Revision != initialSHA || !inlineRv.Author.Bot || inlineRv.Author.Login != "reviewer-agent" {
		t.Errorf("ReviewProposal(inline-only) = %+v", inlineRv)
	}

	// 9. POST /approve 401 when user_can_approve=false (e.g. author self-approval)
	// maps to FORGE_REJECTED with the reason, NOT FORGE_UNAUTHORIZED.
	fx.setMRCannotApprove(prop.Number, true)
	_, err = repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: initialSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "gl-review")
	if !strings.Contains(err.Error(), "user_can_approve=false") {
		t.Errorf("self-approval refusal Detail = %q, want user_can_approve=false", err.Error())
	}
	fx.setMRCannotApprove(prop.Number, false)

	// 10. Silent ReviewApprove records approval; repeating ReviewApprove when
	// already approved (fixture returns 401 on /approve, user_has_approved=true
	// on /approvals) succeeds idempotently.
	approveRv, err := repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: initialSHA,
	})
	if err != nil {
		t.Fatalf("ReviewProposal(silent approve): %v", err)
	}
	if approveRv.ID == "" || approveRv.Decision != vcs.ReviewApprove || approveRv.Revision != initialSHA || approveRv.URL == "" || approveRv.SubmittedAt.IsZero() {
		t.Errorf("ReviewProposal(silent approve) = %+v", approveRv)
	}
	repeatApproveRv, err := repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		ExpectedRevision: initialSHA,
	})
	if err != nil {
		t.Fatalf("ReviewProposal(idempotent repeat approve): %v", err)
	}
	if repeatApproveRv.Decision != vcs.ReviewApprove || repeatApproveRv.Revision != initialSHA {
		t.Errorf("ReviewProposal(idempotent repeat approve) = %+v", repeatApproveRv)
	}

	// 11. ReviewRequestChanges after a prior approval revokes that approval via
	// POST /unapprove so the MR is no longer approved/mergeable; repeating
	// ReviewRequestChanges when already unapproved tolerates 404 Not found.
	reqChangesRv, err := repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewRequestChanges,
		Body:             "hold: verify node memory allocatable first",
		ExpectedRevision: initialSHA,
	})
	if err != nil {
		t.Fatalf("ReviewProposal(request_changes after approve): %v", err)
	}
	if reqChangesRv.Decision != vcs.ReviewRequestChanges {
		t.Errorf("ReviewProposal(request_changes) Decision = %q", reqChangesRv.Decision)
	}
	if fx.snapshotMR(prop.Number).Approved {
		t.Fatal("MR stayed approved after ReviewRequestChanges")
	}
	_, err = repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision: initialSHA,
	})
	vcstest.RequireError(t, err, vcs.CodeProtectedBranch, "gl-review")

	if _, err := repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewRequestChanges,
		Body:             "still waiting on node sizing confirmation",
		ExpectedRevision: initialSHA,
	}); err != nil {
		t.Fatalf("ReviewProposal(request_changes when already unapproved): %v", err)
	}

	// Re-approve before merging.
	if _, err := repo.ReviewProposal(ctx, prop.Number, vcs.ProposalReviewRequest{
		Decision:         vcs.ReviewApprove,
		Body:             "node sizing confirmed, lgtm",
		ExpectedRevision: initialSHA,
	}); err != nil {
		t.Fatalf("ReviewProposal(re-approve): %v", err)
	}

	// 12. MergeProposal with squash, custom commit title/message, and DeleteSourceBranch=true.
	merged, err := repo.MergeProposal(ctx, prop.Number, vcs.ProposalMergeRequest{
		ExpectedRevision:   initialSHA,
		Method:             vcs.MergeSquash,
		CommitTitle:        "feat: raise memory limit (!1)",
		CommitMessage:      "Approved by reviewer-agent.",
		DeleteSourceBranch: true,
	})
	if err != nil {
		t.Fatalf("MergeProposal: %v", err)
	}
	if !merged.Merged || merged.Method != vcs.MergeSquash || merged.Revision != "aaaa111122223333444455556666777788889999" || merged.Proposal.State != vcs.ProposalMerged {
		t.Errorf("MergeProposal = %+v", merged)
	}
	snap := fx.snapshotMR(prop.Number)
	if snap.LastSquash == nil || !*snap.LastSquash {
		t.Errorf("LastSquash = %v, want true", snap.LastSquash)
	}
	if snap.LastSquashCommitMessage != "feat: raise memory limit (!1)\n\nApproved by reviewer-agent." {
		t.Errorf("LastSquashCommitMessage = %q", snap.LastSquashCommitMessage)
	}
	if !snap.RemovedSourceBranch {
		t.Error("RemovedSourceBranch = false, want true")
	}
}

func TestLiveGitLab(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("CORE_VCS_GITLAB_TOKEN"))
	projSpec := strings.TrimSpace(os.Getenv("CORE_VCS_GITLAB_PROJECT"))
	if token == "" || projSpec == "" {
		t.Skip("skipping live GitLab test: set CORE_VCS_GITLAB_TOKEN and CORE_VCS_GITLAB_PROJECT to run")
	}

	ctx := context.Background()
	reg := vcs.NewRegistry()
	reg.MustRegister(gitlab.Name, gitlab.New)
	forge := vcs.Forge{
		Name:     "live-gitlab",
		Provider: gitlab.Name,
		Cred:     vcs.StaticToken(token),
	}
	cat, err := vcs.NewCatalog(reg, []vcs.Forge{forge}, []vcs.RepoConfig{{
		Forge:      "live-gitlab",
		Repository: projSpec,
		Role:       vcs.RoleContext,
	}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	repo, err := cat.Resolve(projSpec)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", projSpec, err)
	}
	caps, err := repo.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.BaseBranch == "" || caps.ProposalNoun != gitlab.ProposalNoun {
		t.Errorf("Capabilities = %+v", caps)
	}
	if _, err := repo.ListProposals(ctx, vcs.ProposalFilter{Page: vcs.Page{Limit: 5}}); err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
}
