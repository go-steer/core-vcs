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

// Package git implements vcs.Provider for a bare smart-HTTP git remote with
// no forge API.
//
// A bare git remote supports clone and publish; every proposal-* verb
// returns FORGE_UNSUPPORTED unless Forge.ProposalURLTemplate is configured,
// in which case Capabilities reports proposalNoun "link" and CreateProposal
// renders {host}, {path}, {base} and {branch} into Proposal{URL} with
// Number == 0.
package git

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/go-steer/core-vcs/vcs"
)

const (
	// Name is the registry name of the bare-git provider.
	Name = "git"
	// DefaultUsername is the HTTP Basic Auth username used when
	// Forge.Username is empty and a credential is present.
	DefaultUsername = "git"
)

// Provider is the bare-git vcs.Provider implementation.
type Provider struct{}

// New constructs a bare-git Provider.
func New() (vcs.Provider, error) {
	return &Provider{}, nil
}

// Name implements vcs.Provider.
func (p *Provider) Name() string { return Name }

// ValidateForge implements vcs.Provider. A bare git remote has no default
// public host, so Host is required.
func (p *Provider) ValidateForge(f vcs.Forge) error {
	h := strings.TrimSpace(f.Host)
	if h == "" {
		return vcs.Errorf(vcs.CodeConfigInvalid, f.Name, "git forge requires Host")
	}
	if _, portStr, hasPort := strings.Cut(h, ":"); hasPort && f.Port != 0 && strconv.Itoa(f.Port) != portStr {
		return vcs.Errorf(vcs.CodeConfigInvalid, f.Name, "Host %q and Port %d specify different ports", f.Host, f.Port)
	}
	switch f.Scheme {
	case "", "https", "http":
	default:
		return vcs.Errorf(vcs.CodeConfigInvalid, f.Name, "scheme %q is not https or http", f.Scheme)
	}
	if tpl := strings.TrimSpace(f.ProposalURLTemplate); tpl != "" {
		if !strings.HasPrefix(tpl, "https://") && !strings.HasPrefix(tpl, "http://") {
			return vcs.Errorf(vcs.CodeConfigInvalid, f.Name, "ProposalURLTemplate %q must start with https:// or http://", f.ProposalURLTemplate)
		}
	}
	return nil
}

// Hosts implements vcs.Provider.
func (p *Provider) Hosts(f vcs.Forge) []string {
	h := strings.ToLower(strings.TrimSpace(f.Host))
	if h == "" {
		return nil
	}
	return []string{h}
}

// Resolve implements vcs.Provider. A bare git remote accepts any host that
// matches the forge and any non-empty opaque path of valid segments.
func (p *Provider) Resolve(f vcs.Forge, ref vcs.RepoRef) (vcs.RepoRef, error) {
	if strings.TrimSpace(ref.Path) == "" {
		return vcs.RepoRef{}, vcs.Errorf(vcs.CodeRepoInvalid, f.Name, "repository path is empty")
	}
	hosts := p.Hosts(f)
	if len(hosts) == 0 {
		return vcs.RepoRef{}, vcs.Errorf(vcs.CodeConfigInvalid, f.Name, "git forge requires Host")
	}
	return vcs.RepoRef{Host: hosts[0], Path: ref.Path}, nil
}

// Capabilities implements vcs.Provider.
func (p *Provider) Capabilities(repo vcs.Repo) vcs.Capabilities {
	if strings.TrimSpace(repo.Forge.ProposalURLTemplate) != "" {
		return vcs.Capabilities{
			ProposalNoun: vcs.ProposalNounLink,
			Verbs:        []vcs.Verb{vcs.VerbProposalCreate},
			Labels:       false,
		}
	}
	return vcs.Capabilities{
		ProposalNoun: vcs.ProposalNounMissing,
		Verbs:        nil,
		Labels:       false,
	}
}

// CloneURL implements vcs.Provider.
func (p *Provider) CloneURL(repo vcs.Repo) string {
	scheme := repo.Forge.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/%s.git", scheme, hostWithPort(repo.Forge), repo.Ref.Path)
}

// CloneAuth implements vcs.Provider.
func (p *Provider) CloneAuth(ctx context.Context, repo vcs.Repo) (*vcs.BasicAuth, error) {
	if repo.Forge.Cred == nil {
		return nil, nil
	}
	tok, err := repo.Forge.Cred.Token(ctx)
	if err != nil {
		return nil, err
	}
	user := strings.TrimSpace(repo.Forge.Username)
	if user == "" {
		user = DefaultUsername
	}
	return &vcs.BasicAuth{Username: user, Password: tok}, nil
}

// BaseBranch implements vcs.Provider by inspecting the remote's advertised
// HEAD reference over the smart-HTTP git protocol.
func (p *Provider) BaseBranch(ctx context.Context, repo vcs.Repo) (string, error) {
	if _, err := vcs.TLSConfig(repo.Forge); err != nil {
		return "", err
	}
	var caBundle []byte
	if repo.Forge.CAFile != "" {
		b, err := os.ReadFile(repo.Forge.CAFile)
		if err != nil {
			return "", vcs.Errorf(vcs.CodeForgeTLSUntrusted, repo.Forge.Name, "CAFile %s: %w", repo.Forge.CAFile, err)
		}
		caBundle = b
	}
	basic, err := p.CloneAuth(ctx, repo)
	if err != nil {
		return "", err
	}
	var auth transport.AuthMethod
	if basic != nil {
		auth = &githttp.BasicAuth{Username: basic.Username, Password: basic.Password}
	}
	rem := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: gogit.DefaultRemoteName,
		URLs: []string{p.CloneURL(repo)},
	})
	refs, err := rem.ListContext(ctx, &gogit.ListOptions{
		Auth:     auth,
		CABundle: caBundle,
	})
	if err != nil {
		return "", classifyListError(repo.Forge, err)
	}

	branches := map[plumbing.ReferenceName]plumbing.Hash{}
	var headRef *plumbing.Reference
	for _, r := range refs {
		if r.Name() == plumbing.HEAD {
			headRef = r
			continue
		}
		if r.Name().IsBranch() && r.Type() == plumbing.HashReference {
			branches[r.Name()] = r.Hash()
		}
	}
	if headRef == nil {
		return "", vcs.Errorf(vcs.CodeBaseBranchMissing, repo.Forge.Name, "remote %s did not advertise HEAD", repo.Ref)
	}
	if headRef.Type() == plumbing.SymbolicReference {
		target := headRef.Target()
		if !target.IsBranch() {
			return "", vcs.Errorf(vcs.CodeBaseBranchMissing, repo.Forge.Name, "remote %s HEAD points to non-branch %s", repo.Ref, target)
		}
		if _, exists := branches[target]; !exists {
			return "", vcs.Errorf(vcs.CodeBaseBranchMissing, repo.Forge.Name, "remote %s HEAD points to unborn branch %s", repo.Ref, target.Short())
		}
		return strings.TrimPrefix(target.String(), "refs/heads/"), nil
	}
	if headRef.Type() == plumbing.HashReference {
		if name := matchBranchByHash(branches, headRef.Hash()); name != "" {
			return name, nil
		}
	}
	return "", vcs.Errorf(vcs.CodeBaseBranchMissing, repo.Forge.Name, "remote %s advertised no default branch for HEAD", repo.Ref)
}

func matchBranchByHash(branches map[plumbing.ReferenceName]plumbing.Hash, want plumbing.Hash) string {
	var matches []string
	for name, h := range branches {
		if h == want {
			matches = append(matches, strings.TrimPrefix(name.String(), "refs/heads/"))
		}
	}
	if len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	for _, preferred := range []string{"main", "master"} {
		for _, m := range matches {
			if m == preferred {
				return m
			}
		}
	}
	return matches[0]
}

func classifyListError(f vcs.Forge, err error) error {
	if errors.Is(err, transport.ErrEmptyRemoteRepository) || errors.Is(err, plumbing.ErrReferenceNotFound) {
		return vcs.Errorf(vcs.CodeBaseBranchMissing, f.Name, "remote repository is empty or has no HEAD: %w", err)
	}
	if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) {
		return vcs.Errorf(vcs.CodeForgeUnauthorized, f.Name, "%w; check the forge credential, do not retry", err)
	}
	return vcs.TransportError(f, "", err)
}

// ProtectedBranch implements vcs.Provider. Branch protection is not
// knowable on a bare git remote ahead of receive-pack.
func (p *Provider) ProtectedBranch(context.Context, vcs.Repo, string) (bool, error) {
	return false, nil
}

// CreateProposal implements vcs.Provider. When Forge.ProposalURLTemplate
// is set, it substitutes {host}, {path}, {base} and {branch} and returns a
// link Proposal with Number == 0.
func (p *Provider) CreateProposal(_ context.Context, repo vcs.Repo, req vcs.ProposalRequest) (vcs.Proposal, error) {
	tpl := strings.TrimSpace(repo.Forge.ProposalURLTemplate)
	if tpl == "" {
		return vcs.Proposal{}, unsupported(repo, vcs.VerbProposalCreate)
	}
	return vcs.Proposal{
		Number: 0,
		URL:    renderProposalURL(tpl, hostWithPort(repo.Forge), repo.Ref.Path, req.Target, req.Source),
		Source: req.Source,
		Target: req.Target,
	}, nil
}

func renderProposalURL(tpl, host, repoPath, base, branch string) string {
	pathPart, queryPart, hasQuery := strings.Cut(tpl, "?")
	pathReplacer := strings.NewReplacer(
		"{host}", host,
		"{path}", escapeSegments(repoPath),
		"{base}", escapeSegments(base),
		"{branch}", escapeSegments(branch),
	)
	rendered := pathReplacer.Replace(pathPart)
	if !hasQuery {
		return rendered
	}
	queryReplacer := strings.NewReplacer(
		"{host}", url.QueryEscape(host),
		"{path}", url.QueryEscape(repoPath),
		"{base}", url.QueryEscape(base),
		"{branch}", url.QueryEscape(branch),
	)
	return rendered + "?" + queryReplacer.Replace(queryPart)
}

func escapeSegments(s string) string {
	segs := strings.Split(s, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}

// ListProposals implements vcs.Provider.
func (p *Provider) ListProposals(_ context.Context, repo vcs.Repo, _ vcs.ProposalFilter) (vcs.ProposalList, error) {
	return vcs.ProposalList{}, unsupported(repo, vcs.VerbProposalList)
}

// GetProposal implements vcs.Provider.
func (p *Provider) GetProposal(_ context.Context, repo vcs.Repo, _ int) (vcs.Proposal, error) {
	return vcs.Proposal{}, unsupported(repo, vcs.VerbProposalView)
}

// ProposalComments implements vcs.Provider.
func (p *Provider) ProposalComments(_ context.Context, repo vcs.Repo, _ int, _ vcs.Page) (vcs.CommentList, error) {
	return vcs.CommentList{}, unsupported(repo, vcs.VerbProposalComments)
}

// ProposalCommits implements vcs.Provider.
func (p *Provider) ProposalCommits(_ context.Context, repo vcs.Repo, _ int, _ vcs.Page) (vcs.CommitList, error) {
	return vcs.CommitList{}, unsupported(repo, vcs.VerbProposalCommits)
}

// CommentProposal implements vcs.Provider.
func (p *Provider) CommentProposal(_ context.Context, repo vcs.Repo, _ int, _ string) (vcs.Comment, error) {
	return vcs.Comment{}, unsupported(repo, vcs.VerbProposalComment)
}

// UpdateProposal implements vcs.Provider.
func (p *Provider) UpdateProposal(_ context.Context, repo vcs.Repo, _ int, _ vcs.ProposalUpdate) (vcs.Proposal, error) {
	return vcs.Proposal{}, unsupported(repo, vcs.VerbProposalUpdate)
}

// CloseProposal implements vcs.Provider.
func (p *Provider) CloseProposal(_ context.Context, repo vcs.Repo, _ int) (vcs.Proposal, error) {
	return vcs.Proposal{}, unsupported(repo, vcs.VerbProposalClose)
}

func unsupported(repo vcs.Repo, verb vcs.Verb) error {
	return vcs.Errorf(vcs.CodeForgeUnsupported, repo.Forge.Name, "forge %q (git) does not support %s", repo.Forge.Name, verb).WithVerb(verb)
}

func hostWithPort(f vcs.Forge) string {
	h := strings.ToLower(strings.TrimSpace(f.Host))
	if f.Port != 0 && !strings.Contains(h, ":") {
		return fmt.Sprintf("%s:%d", h, f.Port)
	}
	return h
}
