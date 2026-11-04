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

package fakeprovider_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/go-steer/core-vcs/internal/fakeprovider"
	"github.com/go-steer/core-vcs/internal/vcstest"
	"github.com/go-steer/core-vcs/vcs"
)

func TestContract(t *testing.T) {
	t.Parallel()

	// Case 1: public host (plus alias), two- and three-segment paths, every
	// verb and every HTTP/status error code.
	pubFx := fakeprovider.NewFixture(t)
	vcstest.Run(t, vcstest.Case{
		Name:        "public-host",
		Forge:       vcs.Forge{Name: "fake-pub", Provider: fakeprovider.Name, Cred: vcs.StaticToken(fakeprovider.ValidToken)},
		Provider:    fakeprovider.New(fakeprovider.Options{Labels: true, DialURL: pubFx.Server.URL}),
		PrimaryRepo: "acme/infra",
		Accept: map[string]string{
			"acme/infra":                                          fakeprovider.PublicHost + "/acme/infra",
			"platform/clusters/prod":                              fakeprovider.PublicHost + "/platform/clusters/prod",
			fakeprovider.PublicHost + "/acme/infra":               fakeprovider.PublicHost + "/acme/infra",
			fakeprovider.PublicAlias + "/acme/infra.git":          fakeprovider.PublicHost + "/acme/infra",
			"https://" + fakeprovider.PublicHost + "/acme/infra/": fakeprovider.PublicHost + "/acme/infra",
			"git@" + fakeprovider.PublicHost + ":acme/infra.git":  fakeprovider.PublicHost + "/acme/infra",
		},
		Reject:  []string{"onlyone", "a/b/c/d"},
		Noun:    "change proposal",
		Serving: vcs.ProposalVerbs,
		Labels:  true,
		Induce:  induceAll(pubFx),
	})

	// Case 2: self-hosted instance over TLS with a private CA bundle, and
	// a custom port extracted from the TLS fixture.
	tlsFx := fakeprovider.NewTLSFixture(t)
	u, err := url.Parse(tlsFx.Server.URL)
	if err != nil {
		t.Fatal(err)
	}
	hostOnly, portStr, _ := strings.Cut(u.Host, ":")
	port, _ := strconv.Atoi(portStr)
	selfHost := "fake.self.internal"

	vcstest.Run(t, vcstest.Case{
		Name: "self-hosted-private-ca",
		Forge: vcs.Forge{
			Name:     "fake-self",
			Provider: fakeprovider.Name,
			Host:     selfHost,
			Port:     port,
			CAFile:   tlsFx.CAFile,
			Cred:     vcs.StaticToken(fakeprovider.ValidToken),
		},
		Provider:    fakeprovider.New(fakeprovider.Options{Labels: true, DialURL: "https://" + hostOnly + ":" + portStr}),
		PrimaryRepo: "platform/clusters/prod",
		Accept: map[string]string{
			"platform/clusters/prod":                              selfHost + "/platform/clusters/prod",
			selfHost + "/platform/clusters/prod":                  selfHost + "/platform/clusters/prod",
			"https://" + selfHost + "/platform/clusters/prod.git": selfHost + "/platform/clusters/prod",
		},
		Reject:  []string{"single", "a/b/c/d"},
		Noun:    "change proposal",
		Serving: vcs.ProposalVerbs,
		Labels:  true,
		Induce:  induceTLS(tlsFx, hostOnly+":"+portStr),
	})

	// Case 3: a link-only configuration (only proposal-create served, no
	// labels), exercising missing-verbs and labels-guard in vcstest.Run.
	linkFx := fakeprovider.NewFixture(t)
	vcstest.Run(t, vcstest.Case{
		Name:        "link-only-no-labels",
		Forge:       vcs.Forge{Name: "fake-link", Provider: fakeprovider.Name, Cred: vcs.StaticToken(fakeprovider.ValidToken)},
		Provider:    fakeprovider.New(fakeprovider.Options{Noun: vcs.ProposalNounLink, Verbs: []vcs.Verb{vcs.VerbProposalCreate}, Labels: false, DialURL: linkFx.Server.URL}),
		PrimaryRepo: "acme/infra",
		Accept: map[string]string{
			"acme/infra": fakeprovider.PublicHost + "/acme/infra",
		},
		Reject:  []string{"onlyone"},
		Noun:    vcs.ProposalNounLink,
		Serving: []vcs.Verb{vcs.VerbProposalCreate},
		Labels:  false,
	})
}

func induceAll(fx *fakeprovider.Fixture) map[vcs.Code]func(t *testing.T, repo vcs.Repo) error {
	ctx := context.Background()
	return map[vcs.Code]func(t *testing.T, repo vcs.Repo) error{
		vcs.CodeForgeUnauthorized: func(_ *testing.T, repo vcs.Repo) error {
			fx.ForceNext(http.StatusUnauthorized, "token expired", nil)
			_, err := repo.ListProposals(ctx, vcs.ProposalFilter{})
			return err
		},
		vcs.CodeForgeUnavailable: func(_ *testing.T, repo vcs.Repo) error {
			fx.ForceNext(http.StatusForbidden, "API rate limit exceeded", map[string]string{"X-RateLimit-Reset": "1793800000"})
			_, err := repo.ListProposals(ctx, vcs.ProposalFilter{})
			return err
		},
		vcs.CodeForgeRejected: func(_ *testing.T, repo vcs.Repo) error {
			fx.ForceNext(http.StatusUnprocessableEntity, "No commits between main and agent/fix", nil)
			_, err := repo.CreateProposal(ctx, vcs.ProposalRequest{Source: "agent/fix", Title: "t"})
			return err
		},
		vcs.CodeForgeCallFailed: func(_ *testing.T, repo vcs.Repo) error {
			fx.ForceNext(http.StatusTeapot, "unexpected upstream proxy body", nil)
			_, err := repo.ListProposals(ctx, vcs.ProposalFilter{})
			return err
		},
		vcs.CodeBaseBranchMissing: func(_ *testing.T, repo vcs.Repo) error {
			fx.SetBaseBranch("")
			defer fx.SetBaseBranch("main")
			_, err := repo.BaseBranchName(ctx)
			return err
		},
	}
}

func induceTLS(fx *fakeprovider.Fixture, addr string) map[vcs.Code]func(t *testing.T, repo vcs.Repo) error {
	m := induceAll(fx)
	m[vcs.CodeForgeTLSUntrusted] = func(t *testing.T, repo vcs.Repo) error {
		reg := vcs.NewRegistry()
		reg.MustRegister(fakeprovider.Name, fakeprovider.New(fakeprovider.Options{DialURL: "https://" + addr}))
		noCA := repo.Forge
		noCA.CAFile = ""
		cat, err := vcs.NewCatalog(reg, []vcs.Forge{noCA}, []vcs.RepoConfig{{
			Forge:      noCA.Name,
			Repository: repo.Ref.Path,
			Role:       vcs.RoleManaged,
		}})
		if err != nil {
			t.Fatalf("NewCatalog: %v", err)
		}
		r, _ := cat.Resolve(repo.Ref.Path)
		_, err = r.ListProposals(context.Background(), vcs.ProposalFilter{})
		return err
	}
	return m
}
