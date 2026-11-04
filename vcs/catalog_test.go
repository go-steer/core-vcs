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
	"testing"

	"github.com/go-steer/core-vcs/internal/fakeprovider"
	"github.com/go-steer/core-vcs/internal/vcstest"
	"github.com/go-steer/core-vcs/vcs"
)

func testRegistry() *vcs.Registry {
	reg := vcs.NewRegistry()
	reg.MustRegister(fakeprovider.Name, fakeprovider.New(fakeprovider.Options{Labels: true}))
	return reg
}

func TestRegistryValidation(t *testing.T) {
	t.Parallel()
	reg := vcs.NewRegistry()
	vcstest.RequireError(t, reg.Register("", fakeprovider.New(fakeprovider.Options{})), vcs.CodeConfigInvalid, "")
	vcstest.RequireError(t, reg.Register("x", nil), vcs.CodeConfigInvalid, "")

	reg.MustRegister(fakeprovider.Name, fakeprovider.New(fakeprovider.Options{}))
	vcstest.RequireError(t, reg.Register(fakeprovider.Name, fakeprovider.New(fakeprovider.Options{})), vcs.CodeConfigInvalid, "")

	_, err := reg.Provider("unknown")
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "")

	// A provider registered under a name that disagrees with its own Name().
	reg.MustRegister("wrong", fakeprovider.New(fakeprovider.Options{}))
	_, err = reg.Provider("wrong")
	vcstest.RequireError(t, err, vcs.CodeConfigInvalid, "")
}

func TestNewCatalogValidation(t *testing.T) {
	t.Parallel()
	reg := testRegistry()
	f1 := vcs.Forge{Name: "f1", Provider: fakeprovider.Name}
	f2 := vcs.Forge{Name: "f2", Provider: fakeprovider.Name, Host: "self.corp.example"}

	cases := []struct {
		name   string
		forges []vcs.Forge
		repos  []vcs.RepoConfig
		want   vcs.Code
	}{
		{
			name:   "empty forge name",
			forges: []vcs.Forge{{Provider: fakeprovider.Name}},
			want:   vcs.CodeConfigInvalid,
		},
		{
			name:   "duplicate forge name",
			forges: []vcs.Forge{f1, f1},
			want:   vcs.CodeConfigInvalid,
		},
		{
			name:   "port out of range",
			forges: []vcs.Forge{{Name: "f", Provider: fakeprovider.Name, Host: "h.example", Port: 70000}},
			want:   vcs.CodeConfigInvalid,
		},
		{
			name:   "unknown provider",
			forges: []vcs.Forge{{Name: "f", Provider: "bitbucket"}},
			want:   vcs.CodeForgeUnsupported,
		},
		{
			name:   "provider forge rule (CAFile on public host)",
			forges: []vcs.Forge{{Name: "f", Provider: fakeprovider.Name, CAFile: "/etc/ca.pem"}},
			want:   vcs.CodeConfigInvalid,
		},
		{
			name: "two forges claim the same host",
			forges: []vcs.Forge{
				{Name: "a", Provider: fakeprovider.Name, Host: "shared.corp.example"},
				{Name: "b", Provider: fakeprovider.Name, Host: "shared.corp.example"},
			},
			want: vcs.CodeConfigInvalid,
		},
		{
			name:   "repo names unknown forge",
			forges: []vcs.Forge{f1},
			repos:  []vcs.RepoConfig{{Forge: "nope", Repository: "acme/infra", Role: vcs.RoleManaged}},
			want:   vcs.CodeConfigInvalid,
		},
		{
			name:   "invalid role",
			forges: []vcs.Forge{f1},
			repos:  []vcs.RepoConfig{{Forge: "f1", Repository: "acme/infra", Role: "admin"}},
			want:   vcs.CodeConfigInvalid,
		},
		{
			name:   "base_branch on context role",
			forges: []vcs.Forge{f1},
			repos:  []vcs.RepoConfig{{Forge: "f1", Repository: "acme/infra", Role: vcs.RoleContext, BaseBranch: "main"}},
			want:   vcs.CodeConfigInvalid,
		},
		{
			name:   "repo host belongs to a different forge",
			forges: []vcs.Forge{f1, f2},
			repos:  []vcs.RepoConfig{{Forge: "f1", Repository: "https://self.corp.example/acme/infra", Role: vcs.RoleManaged}},
			want:   vcs.CodeForgeUnsupported,
		},
		{
			name:   "schemeless repo on 3-segment forge starts with another forge's host",
			forges: []vcs.Forge{f1, f2},
			repos:  []vcs.RepoConfig{{Forge: "f2", Repository: fakeprovider.PublicHost + "/acme/infra", Role: vcs.RoleManaged}},
			want:   vcs.CodeForgeUnsupported,
		},
		{
			name:   "duplicate repository",
			forges: []vcs.Forge{f1},
			repos: []vcs.RepoConfig{
				{Forge: "f1", Repository: "acme/infra", Role: vcs.RoleManaged},
				{Forge: "f1", Repository: "https://" + fakeprovider.PublicHost + "/acme/infra.git", Role: vcs.RoleContext},
			},
			want: vcs.CodeConfigInvalid,
		},
		{
			name:   "two gitops roles",
			forges: []vcs.Forge{f1},
			repos: []vcs.RepoConfig{
				{Forge: "f1", Repository: "acme/one", Role: vcs.RoleGitOps},
				{Forge: "f1", Repository: "acme/two", Role: vcs.RoleGitOps},
			},
			want: vcs.CodeConfigInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := vcs.NewCatalog(reg, tc.forges, tc.repos)
			vcstest.RequireError(t, err, tc.want, "")
		})
	}
}

func TestCatalogResolveAndHostIsolation(t *testing.T) {
	t.Parallel()
	reg := testRegistry()
	forges := []vcs.Forge{
		{Name: "pub", Provider: fakeprovider.Name},
		{Name: "corp", Provider: fakeprovider.Name, Host: "git.corp.example"},
	}
	repos := []vcs.RepoConfig{
		{Forge: "pub", Repository: "acme/infra", Role: vcs.RoleGitOps, BaseBranch: "main"},
		{Forge: "corp", Repository: "platform/clusters/prod", Role: vcs.RoleManaged},
		{Forge: "pub", Repository: "shared/lib", Role: vcs.RoleContext},
		{Forge: "corp", Repository: "shared/lib", Role: vcs.RoleContext},
	}
	cat, err := vcs.NewCatalog(reg, forges, repos)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	if g, ok := cat.GitOps(); !ok || g.Ref.Path != "acme/infra" {
		t.Fatalf("GitOps() = (%+v, %v), want acme/infra", g, ok)
	}

	// Host-less path listed on exactly one forge resolves to that forge
	// even when two forges are configured.
	r, err := cat.Resolve("platform/clusters/prod")
	if err != nil || r.Forge.Name != "corp" {
		t.Fatalf("Resolve(platform/clusters/prod) = (%+v, %v), want corp", r, err)
	}

	// Host-less path listed on BOTH forges is ambiguous -> FORGE_UNSUPPORTED.
	_, err = cat.Resolve("shared/lib")
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "")
	// Naming the host disambiguates it.
	if r, err := cat.Resolve("git.corp.example/shared/lib"); err != nil || r.Forge.Name != "corp" {
		t.Fatalf("Resolve(git.corp.example/shared/lib) = (%+v, %v)", r, err)
	}

	// Host-less path listed on NEITHER forge, with two forges configured ->
	// FORGE_UNSUPPORTED ("name the host").
	_, err = cat.Resolve("acme/unlisted")
	vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "")

	// With the host named (or with a single forge), an unlisted repository
	// on a configured forge is REPO_UNKNOWN.
	_, err = cat.Resolve("git.corp.example/acme/unlisted")
	vcstest.RequireError(t, err, vcs.CodeRepoUnknown, "corp")

	// NewRepo is the explicit door to an unlisted repository.
	adhoc, err := cat.NewRepo(vcs.RepoConfig{
		Forge:      "corp",
		Repository: "acme/unlisted",
		Role:       vcs.RoleContext,
	})
	if err != nil || adhoc.String() != "git.corp.example/acme/unlisted" {
		t.Fatalf("NewRepo = (%+v, %v)", adhoc, err)
	}

	// kube-agents #1085 host-confusion strings: every one must be refused
	// with FORGE_UNSUPPORTED, never matched by substring to pub.
	hostConfusion := []string{
		"https://evil.example/acme/infra.git",
		"https://" + fakeprovider.PublicHost + ".evil.example/acme/infra.git",
		"https://evil.example/" + fakeprovider.PublicHost + "/acme/infra.git",
		"git@evil.example:acme/infra.git",
	}
	for _, bad := range hostConfusion {
		_, err := cat.Resolve(bad)
		vcstest.RequireError(t, err, vcs.CodeForgeUnsupported, "")
	}
}
