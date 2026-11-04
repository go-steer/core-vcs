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

package vcs

import (
	"fmt"
	"sort"
	"strings"
)

// Catalog is the validated set of forges and repositories an install
// configured. It is the resolution entry point: a value a caller holds
// becomes a Repo here or is refused with a code, and every configuration
// rule is checked once, in NewCatalog, so a consumer has no second
// validator to keep in step.
//
// There is no implicit fallback. A host no forge serves is
// FORGE_UNSUPPORTED; a repository on a configured forge that is not
// listed is REPO_UNKNOWN. NewRepo is the one door to an unlisted
// repository, and it takes an explicit RepoConfig.
type Catalog struct {
	forges    map[string]*forgeEntry
	order     []string          // forge names in configuration order
	hostOwner map[string]string // host (and host:port) -> forge name
	repos     []Repo            // listed, in configuration order
	index     map[string]int    // repoKey -> position in repos
}

type forgeEntry struct {
	forge    Forge
	provider Provider
	hosts    []string // canonical first; includes host:port forms
}

// NewCatalog validates forges and repos against reg and returns the
// catalog. Every failure is an *Error with CONFIG_INVALID, REPO_INVALID
// or FORGE_UNSUPPORTED and a Detail that names the entry.
func NewCatalog(reg *Registry, forges []Forge, repos []RepoConfig) (*Catalog, error) {
	if reg == nil {
		return nil, Errorf(CodeConfigInvalid, "", "nil registry")
	}
	c := &Catalog{
		forges:    map[string]*forgeEntry{},
		hostOwner: map[string]string{},
		index:     map[string]int{},
	}
	providers := map[string]Provider{}
	for _, f := range forges {
		if err := c.addForge(reg, providers, f); err != nil {
			return nil, err
		}
	}
	gitops := 0
	for i, cfg := range repos {
		repo, err := c.NewRepo(cfg)
		if err != nil {
			return nil, Errorf(CodeOf(err), cfg.Forge, "repositories[%d]: %w", i, err)
		}
		key := repoKey(repo.Forge.Name, repo.Ref)
		if j, dup := c.index[key]; dup {
			return nil, Errorf(CodeConfigInvalid, cfg.Forge, "repositories[%d] and [%d] both name %s on forge %q", j, i, repo.Ref, cfg.Forge)
		}
		if repo.Role == RoleGitOps {
			gitops++
			if gitops > 1 {
				return nil, Errorf(CodeConfigInvalid, cfg.Forge, "repositories[%d]: a second %q role; at most one repository is gitops", i, RoleGitOps)
			}
		}
		c.index[key] = len(c.repos)
		c.repos = append(c.repos, repo)
	}
	return c, nil
}

func (c *Catalog) addForge(reg *Registry, providers map[string]Provider, f Forge) error {
	if f.Name == "" {
		return Errorf(CodeConfigInvalid, "", "forge with provider %q has no name", f.Provider)
	}
	if _, dup := c.forges[f.Name]; dup {
		return Errorf(CodeConfigInvalid, f.Name, "forge %q configured twice", f.Name)
	}
	if f.Port < 0 || f.Port > 65535 {
		return Errorf(CodeConfigInvalid, f.Name, "forge %q: port %d out of range", f.Name, f.Port)
	}
	p, ok := providers[f.Provider]
	if !ok {
		var err error
		if p, err = reg.Provider(f.Provider); err != nil {
			return Errorf(CodeOf(err), f.Name, "forge %q: %w", f.Name, err)
		}
		providers[f.Provider] = p
	}
	if err := p.ValidateForge(f); err != nil {
		code := CodeOf(err)
		if code == "" {
			code = CodeConfigInvalid
		}
		return Errorf(code, f.Name, "forge %q: %w", f.Name, err)
	}
	hosts, err := forgeHosts(p, f)
	if err != nil {
		return err
	}
	for _, h := range hosts {
		if owner, taken := c.hostOwner[h]; taken {
			return Errorf(CodeConfigInvalid, f.Name, "host %q is claimed by forges %q and %q; a host has one forge", h, owner, f.Name)
		}
	}
	for _, h := range hosts {
		c.hostOwner[h] = f.Name
	}
	c.forges[f.Name] = &forgeEntry{forge: f, provider: p, hosts: hosts}
	c.order = append(c.order, f.Name)
	return nil
}

// forgeHosts is the provider's host list, lower-cased, with a host:port
// form added for each bare host when the forge sets a port.
func forgeHosts(p Provider, f Forge) ([]string, error) {
	raw := p.Hosts(f)
	if len(raw) == 0 {
		return nil, Errorf(CodeConfigInvalid, f.Name, "forge %q: provider %q answers no hosts", f.Name, p.Name())
	}
	var hosts []string
	seen := map[string]bool{}
	add := func(h string) {
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	for _, h := range raw {
		h = strings.ToLower(strings.TrimSpace(h))
		if !validHost(h) {
			return nil, Errorf(CodeConfigInvalid, f.Name, "forge %q: host %q is not a host name", f.Name, h)
		}
		add(h)
		if f.Port != 0 && !strings.Contains(h, ":") {
			add(fmt.Sprintf("%s:%d", h, f.Port))
		}
	}
	return hosts, nil
}

func repoKey(forge string, ref RepoRef) string { return forge + "\x00" + ref.String() }

// Forge returns the named forge.
func (c *Catalog) Forge(name string) (Forge, bool) {
	e, ok := c.forges[name]
	if !ok {
		return Forge{}, false
	}
	return e.forge, true
}

// Forges returns the forges in configuration order.
func (c *Catalog) Forges() []Forge {
	out := make([]Forge, 0, len(c.order))
	for _, n := range c.order {
		out = append(out, c.forges[n].forge)
	}
	return out
}

// Repos returns the listed repositories in configuration order.
func (c *Catalog) Repos() []Repo {
	out := make([]Repo, len(c.repos))
	copy(out, c.repos)
	return out
}

// GitOps returns the one RoleGitOps repository, if configured.
func (c *Catalog) GitOps() (Repo, bool) {
	for _, r := range c.repos {
		if r.Role == RoleGitOps {
			return r, true
		}
	}
	return Repo{}, false
}

// Hosts returns every host the catalog serves, sorted.
func (c *Catalog) Hosts() []string {
	hosts := make([]string, 0, len(c.hostOwner))
	for h := range c.hostOwner {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}

// Resolve turns a value a caller holds into a listed Repo.
//
// A value with a host resolves on the forge that owns the host, or is
// FORGE_UNSUPPORTED. A value with no host resolves to the one listed
// repository whose canonical path it matches; with no such match it
// resolves on the only configured forge, and with two or more forges it
// is FORGE_UNSUPPORTED and the Detail says to name the host. A repository
// that resolves on a forge but is not listed is REPO_UNKNOWN.
func (c *Catalog) Resolve(value string) (Repo, error) {
	ref, err := ParseRepoRef(value, c.Hosts()...)
	if err != nil {
		return Repo{}, err
	}
	var entry *forgeEntry
	if ref.Host != "" {
		owner, ok := c.hostOwner[ref.Host]
		if !ok {
			return Repo{}, Errorf(CodeForgeUnsupported, "", "host %q is not a configured forge; configure it or name a listed repository", ref.Host)
		}
		entry = c.forges[owner]
	} else {
		entry, err = c.forgeForPath(ref)
		if err != nil {
			return Repo{}, err
		}
	}
	return c.lookup(entry, ref)
}

// forgeForPath chooses the forge for a host-less value.
func (c *Catalog) forgeForPath(ref RepoRef) (*forgeEntry, error) {
	var matches []*forgeEntry
	for _, name := range c.order {
		e := c.forges[name]
		canon, err := e.provider.Resolve(e.forge, RepoRef{Host: e.hosts[0], Path: ref.Path})
		if err != nil {
			continue
		}
		if _, listed := c.index[repoKey(name, canon)]; listed {
			matches = append(matches, e)
		}
	}
	switch {
	case len(matches) == 1:
		return matches[0], nil
	case len(matches) > 1:
		names := make([]string, len(matches))
		for i, e := range matches {
			names[i] = e.forge.Name
		}
		return nil, Errorf(CodeForgeUnsupported, "", "%q is listed on forges %v; name the host", ref.Path, names)
	case len(c.order) == 1:
		return c.forges[c.order[0]], nil
	case len(c.order) == 0:
		return nil, Errorf(CodeForgeUnsupported, "", "no forge is configured")
	default:
		return nil, Errorf(CodeForgeUnsupported, "", "%q names no host and %d forges are configured; name the host", ref.Path, len(c.order))
	}
}

func (c *Catalog) lookup(e *forgeEntry, ref RepoRef) (Repo, error) {
	ref.Host = e.hosts[0]
	canon, err := e.provider.Resolve(e.forge, ref)
	if err != nil {
		return Repo{}, coded(err, CodeRepoInvalid, e.forge.Name)
	}
	i, ok := c.index[repoKey(e.forge.Name, canon)]
	if !ok {
		return Repo{}, Errorf(CodeRepoUnknown, e.forge.Name, "%s is on forge %q but not in repositories; list it with a role", canon, e.forge.Name)
	}
	return c.repos[i], nil
}

// NewRepo builds a Repo from an explicit RepoConfig without listing it.
// It applies every per-entry rule NewCatalog applies: the forge must
// exist, the role must be valid, a RoleContext entry may not set
// BaseBranch, a host in the value must be the forge's, and the provider's
// grammar must accept the path.
func (c *Catalog) NewRepo(cfg RepoConfig) (Repo, error) {
	e, ok := c.forges[cfg.Forge]
	if !ok {
		return Repo{}, Errorf(CodeConfigInvalid, cfg.Forge, "forge %q is not configured; known: %v", cfg.Forge, c.order)
	}
	if !cfg.Role.Valid() {
		return Repo{}, Errorf(CodeConfigInvalid, cfg.Forge, "role %q is not %q, %q or %q", cfg.Role, RoleGitOps, RoleManaged, RoleContext)
	}
	baseBranch := strings.TrimPrefix(strings.TrimSpace(cfg.BaseBranch), "refs/heads/")
	if cfg.Role == RoleContext && baseBranch != "" {
		return Repo{}, Errorf(CodeConfigInvalid, cfg.Forge, "%q sets base_branch %q on a %q entry; a read-only repository has no target", cfg.Repository, cfg.BaseBranch, RoleContext)
	}
	ref, err := ParseRepoRef(cfg.Repository, c.Hosts()...)
	if err != nil {
		return Repo{}, coded(err, CodeRepoInvalid, cfg.Forge)
	}
	if ref.Host != "" && c.hostOwner[ref.Host] != e.forge.Name {
		return Repo{}, Errorf(CodeForgeUnsupported, cfg.Forge, "%q names host %q, which forge %q does not serve", cfg.Repository, ref.Host, cfg.Forge)
	}
	ref.Host = e.hosts[0]
	canon, err := e.provider.Resolve(e.forge, ref)
	if err != nil {
		return Repo{}, coded(err, CodeRepoInvalid, cfg.Forge)
	}
	if canon.Host == "" || canon.Path == "" {
		return Repo{}, Errorf(CodeConfigInvalid, cfg.Forge, "provider %q resolved %q to an empty ref", e.provider.Name(), cfg.Repository)
	}
	return Repo{
		Forge:      e.forge,
		Ref:        canon,
		Role:       cfg.Role,
		BaseBranch: baseBranch,
		provider:   e.provider,
	}, nil
}

// coded makes sure an error leaving the catalog is an *Error with a code
// and a forge name.
func coded(err error, fallback Code, forge string) error {
	if e, ok := err.(*Error); ok { //nolint:errorlint // we want the outermost only
		if e.Forge == "" {
			return Errorf(e.Code, forge, "%w", e)
		}
		return e
	}
	if code := CodeOf(err); code != "" {
		return Errorf(code, forge, "%w", err)
	}
	return Errorf(fallback, forge, "%w", err)
}
