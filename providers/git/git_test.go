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

package git_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-steer/core-vcs/internal/gitremote"
	"github.com/go-steer/core-vcs/internal/vcstest"
	"github.com/go-steer/core-vcs/providers/git"
	"github.com/go-steer/core-vcs/vcs"
)

func remoteHostPort(tb testing.TB, rawURL string) (string, int) {
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

func TestContract(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		template string
		noun     string
		serving  []vcs.Verb
	}{
		{
			name:     "bare-remote-missing",
			template: "",
			noun:     vcs.ProposalNounMissing,
			serving:  nil,
		},
		{
			name:     "bare-remote-link",
			template: "https://{host}/{path}/compare/{base}...{branch}",
			noun:     vcs.ProposalNounLink,
			serving:  []vcs.Verb{vcs.VerbProposalCreate},
		},
	} {
		rem := gitremote.NewTLS(t, "acme/infra", nil)
		rem.RequireAuth("bot-user", "secret-git-token")
		host, port := remoteHostPort(t, rem.Server.URL)
		portStr := strconv.Itoa(port)

		forge := vcs.Forge{
			Name:                "corp-git",
			Provider:            git.Name,
			Host:                host,
			Scheme:              "https",
			Port:                port,
			CAFile:              rem.CAFile,
			Username:            "bot-user",
			ProposalURLTemplate: tc.template,
			Cred:                vcs.StaticToken("secret-git-token"),
		}

		vcstest.Run(t, vcstest.Case{
			Name:        tc.name,
			Forge:       forge,
			Provider:    git.New,
			PrimaryRepo: "acme/infra",
			Accept: map[string]string{
				"acme/infra":                         host + "/acme/infra",
				host + "/acme/infra":                 host + "/acme/infra",
				host + ":" + portStr + "/acme/infra": host + "/acme/infra",
				"https://" + host + ":" + portStr + "/acme/infra.git": host + "/acme/infra",
				"git@" + host + ":acme/infra.git":                     host + "/acme/infra",
				"group/sub/deep/project":                              host + "/group/sub/deep/project",
				"single-repo":                                         host + "/single-repo",
			},
			Noun:    tc.noun,
			Serving: tc.serving,
			Labels:  false,
			Induce: map[vcs.Code]func(t *testing.T, repo vcs.Repo) error{
				vcs.CodeBaseBranchMissing: func(t *testing.T, repo vcs.Repo) error {
					rem.SetHEAD(t, "unborn-branch")
					defer rem.SetHEAD(t, "main")
					_, err := repo.BaseBranchName(context.Background())
					return err
				},
				vcs.CodeForgeUnauthorized: func(t *testing.T, repo vcs.Repo) error {
					rem.RequireAuth("bot-user", "rotated-token")
					defer rem.RequireAuth("bot-user", "secret-git-token")
					_, err := repo.BaseBranchName(context.Background())
					return err
				},
				vcs.CodeForgeTLSUntrusted: func(t *testing.T, _ vcs.Repo) error {
					reg := vcs.NewRegistry()
					reg.MustRegister(git.Name, git.New)
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
				},
				vcs.CodeForgeRejected: func(t *testing.T, repo vcs.Repo) error {
					_, err := repo.ProtectedBranch(context.Background(), "   ")
					return err
				},
			},
		})
	}
}

func TestValidateForge(t *testing.T) {
	t.Parallel()
	p, err := git.New()
	if err != nil {
		t.Fatalf("git.New: %v", err)
	}

	vcstest.RequireError(t, p.ValidateForge(vcs.Forge{Name: "g", Provider: git.Name}), vcs.CodeConfigInvalid, "g")
	vcstest.RequireError(t, p.ValidateForge(vcs.Forge{Name: "g", Provider: git.Name, Host: "git.example:8443", Port: 9443}), vcs.CodeConfigInvalid, "g")
	vcstest.RequireError(t, p.ValidateForge(vcs.Forge{Name: "g", Provider: git.Name, Host: "git.example", Scheme: "ssh"}), vcs.CodeConfigInvalid, "g")
	vcstest.RequireError(t, p.ValidateForge(vcs.Forge{
		Name:                "g",
		Provider:            git.Name,
		Host:                "git.example",
		ProposalURLTemplate: "ftp://{host}/{path}",
	}), vcs.CodeConfigInvalid, "g")

	if _, err := p.Resolve(vcs.Forge{Name: "g", Provider: git.Name, Host: "git.example"}, vcs.RepoRef{Host: "git.example", Path: ""}); err == nil {
		t.Fatal("Resolve with empty path succeeded, want REPO_INVALID")
	}
}

func TestClonePublishRejectedAndProposalLink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rem := gitremote.NewTLS(t, "platform/manifests", map[string]string{
		"deploy.yaml": "replicas: 2\n",
	})
	rem.RequireAuth(git.DefaultUsername, "tok-123")
	host, port := remoteHostPort(t, rem.Server.URL)

	reg := vcs.NewRegistry()
	reg.MustRegister(git.Name, git.New)
	forge := vcs.Forge{
		Name:                "bare",
		Provider:            git.Name,
		Host:                host,
		Scheme:              "https",
		Port:                port,
		CAFile:              rem.CAFile,
		ProposalURLTemplate: "https://{host}/{path}/+/refs/heads/{branch}?base={base}",
		Cred:                vcs.StaticToken("tok-123"),
	}
	cat, err := vcs.NewCatalog(reg, []vcs.Forge{forge}, []vcs.RepoConfig{{
		Forge:      "bare",
		Repository: "platform/manifests",
		Role:       vcs.RoleGitOps,
	}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	repo, err := cat.Resolve("platform/manifests")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// ProtectedBranch is not knowable on a bare git remote.
	prot, err := repo.ProtectedBranch(ctx, "main")
	if err != nil || prot {
		t.Fatalf("ProtectedBranch(main) = (%v, %v), want (false, nil)", prot, err)
	}

	dir := filepath.Join(t.TempDir(), "work")
	ws, err := vcs.Clone(ctx, repo, "", dir, vcs.Policy{
		BranchPrefix: "agent/",
	})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := ws.Branch("agent/scale-up"); err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy.yaml"), []byte("replicas: 4\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	id := vcs.Identity{Name: "Agent", Email: "agent@example"}
	if _, err := ws.Commit("feat: scale replicas to 4", id); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Server-side receive-pack rejection maps to FORGE_REJECTED with the
	// server's message preserved in Detail.
	rem.RejectNextPush("pre-receive hook declined: signed-off-by required")
	_, err = ws.Publish(ctx, "agent/scale-up")
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "bare")
	if !strings.Contains(err.Error(), "pre-receive hook declined: signed-off-by required") {
		t.Errorf("Publish error = %q, want server message in Detail", err.Error())
	}

	// Subsequent publish succeeds.
	if _, err := ws.Publish(ctx, "agent/scale-up"); err != nil {
		t.Fatalf("Publish after unblocking: %v", err)
	}

	prop, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "agent/scale-up",
		Title:  "feat: scale replicas to 4",
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	wantURL := "https://" + host + ":" + strconv.Itoa(port) + "/platform/manifests/+/refs/heads/agent/scale-up?base=main"
	if prop.Number != 0 || prop.URL != wantURL || prop.Target != "main" || prop.Source != "agent/scale-up" {
		t.Errorf("CreateProposal = %+v, want Number=0 URL=%q Target=main Source=agent/scale-up", prop, wantURL)
	}

	// A branch containing '&' cannot inject extra query parameters when
	// rendered into the query portion of ProposalURLTemplate.
	remWithQuery := forge
	remWithQuery.ProposalURLTemplate = "https://{host}/{path}/new?base={base}&head={branch}"
	catQ, err := vcs.NewCatalog(reg, []vcs.Forge{remWithQuery}, []vcs.RepoConfig{{
		Forge:      "bare",
		Repository: "platform/manifests",
		Role:       vcs.RoleGitOps,
	}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	repoQ, err := catQ.Resolve("platform/manifests")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	propQ, err := repoQ.CreateProposal(ctx, vcs.ProposalRequest{
		Source: "agent/a&evil=1",
		Title:  "feat: test query escaping",
	})
	if err != nil {
		t.Fatalf("CreateProposal query escaping: %v", err)
	}
	if !strings.HasSuffix(propQ.URL, "?base=main&head=agent%2Fa%26evil%3D1") {
		t.Errorf("CreateProposal query-escaped URL = %q, want escaped '&' and '=' in head param", propQ.URL)
	}
}

func TestAnonymousHTTPCloneAndDirectProviderUnsupported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rem := gitremote.New(t, "anon/repo", map[string]string{"hello.txt": "hi\n"})
	host, port := remoteHostPort(t, rem.Server.URL)

	reg := vcs.NewRegistry()
	reg.MustRegister(git.Name, git.New)
	forge := vcs.Forge{
		Name:     "anon-git",
		Provider: git.Name,
		Host:     host,
		Scheme:   "http",
		Port:     port,
	}
	cat, err := vcs.NewCatalog(reg, []vcs.Forge{forge}, []vcs.RepoConfig{{
		Forge:      "anon-git",
		Repository: "anon/repo",
		Role:       vcs.RoleContext,
	}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	repo, err := cat.Resolve("anon/repo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	auth, err := repo.CloneAuth(ctx)
	if err != nil || auth != nil {
		t.Fatalf("CloneAuth = (%+v, %v), want (nil, nil)", auth, err)
	}
	base, err := repo.BaseBranchName(ctx)
	if err != nil || base != "main" {
		t.Fatalf("BaseBranchName = (%q, %v), want (\"main\", nil)", base, err)
	}

	// Direct Provider proposal methods return FORGE_UNSUPPORTED even if
	// called outside Repo's capability gate.
	p, err := git.New()
	if err != nil {
		t.Fatalf("git.New: %v", err)
	}
	_, err = p.CreateProposal(ctx, repo, vcs.ProposalRequest{Source: "agent/x", Target: "main"})
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "anon-git")
	_, err = p.ListProposals(ctx, repo, vcs.ProposalFilter{})
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "anon-git")
	_, err = p.GetProposal(ctx, repo, 1)
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "anon-git")
	_, err = p.ProposalComments(ctx, repo, 1, vcs.Page{})
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "anon-git")
	_, err = p.ProposalCommits(ctx, repo, 1, vcs.Page{})
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "anon-git")
	_, err = p.CommentProposal(ctx, repo, 1, "note")
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "anon-git")
	_, err = p.UpdateProposal(ctx, repo, 1, vcs.ProposalUpdate{})
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "anon-git")
	_, err = p.CloseProposal(ctx, repo, 1)
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "anon-git")
}
