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

// Package github implements vcs.Provider for github.com and GitHub
// Enterprise Server (GHES) over net/http with no third-party SDK.
//
// All GitHub-specific host names, API base URL rules (https://api.github.com
// vs https://<host>/api/v3), media types and clone basic-auth conventions
// live inside this package.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-steer/core-vcs/vcs"
)

const (
	// Name is the registry name of the GitHub provider.
	Name = "github"
	// PublicHost is the canonical public GitHub host.
	PublicHost = "github.com"
	// PublicWWWHost is the www alias of PublicHost.
	PublicWWWHost = "www.github.com"
	// PublicSSHHost is the SSH over port 443 alias of PublicHost.
	PublicSSHHost = "ssh.github.com"
	// PublicAPIBaseURL is the REST API base URL for github.com.
	PublicAPIBaseURL = "https://api.github.com"
	// EnterpriseAPIPrefix is the REST API path prefix on GitHub Enterprise Server.
	EnterpriseAPIPrefix = "/api/v3"
	// CloneUsername is the HTTP Basic Auth username GitHub requires when
	// authenticating smart-HTTP git operations with a token.
	CloneUsername = "x-access-token"
	// AcceptHeader is the GitHub REST v3 media type.
	AcceptHeader = "application/vnd.github+json"
	// APIVersion is the pinned GitHub REST API version header value.
	APIVersion = "2022-11-28"
	// ProposalNoun is what GitHub calls a proposal.
	ProposalNoun = "pull request"

	defaultPerPage   = 30
	maxPerPage       = 100
	maxResponseBytes = 16 << 20 // 16 MiB ceiling on API response bodies
)

// Options configures a Provider instance for testing. Production callers
// use New.
type Options struct {
	// PublicAPIBaseURL overrides PublicAPIBaseURL when Forge.Host targets
	// github.com, allowing contract tests to point github.com API traffic at
	// an httptest server without changing Forge.Host.
	PublicAPIBaseURL string
}

// Provider is the GitHub vcs.Provider implementation.
type Provider struct {
	opts Options
}

// New constructs a GitHub Provider with default production settings.
func New() (vcs.Provider, error) {
	return &Provider{}, nil
}

// NewWithOptions constructs a GitHub Provider with opts.
func NewWithOptions(opts Options) vcs.Constructor {
	return func() (vcs.Provider, error) {
		return &Provider{opts: opts}, nil
	}
}

// Name implements vcs.Provider.
func (p *Provider) Name() string { return Name }

// ValidateForge implements vcs.Provider.
func (p *Provider) ValidateForge(f vcs.Forge) error {
	h := strings.ToLower(strings.TrimSpace(f.Host))
	if isPublicHost(h) && f.CAFile != "" {
		return vcs.Errorf(vcs.CodeConfigInvalid, f.Name, "CAFile is only valid when Host names a GitHub Enterprise Server instance, not %s", PublicHost)
	}
	if _, portStr, hasPort := strings.Cut(h, ":"); hasPort && f.Port != 0 && strconv.Itoa(f.Port) != portStr {
		return vcs.Errorf(vcs.CodeConfigInvalid, f.Name, "Host %q and Port %d specify different ports", f.Host, f.Port)
	}
	switch f.Scheme {
	case "", "https", "http":
	default:
		return vcs.Errorf(vcs.CodeConfigInvalid, f.Name, "scheme %q is not https or http", f.Scheme)
	}
	if f.ProposalURLTemplate != "" {
		return vcs.Errorf(vcs.CodeConfigInvalid, f.Name, "ProposalURLTemplate is only valid on the bare git provider")
	}
	return nil
}

// Hosts implements vcs.Provider.
func (p *Provider) Hosts(f vcs.Forge) []string {
	h := strings.ToLower(strings.TrimSpace(f.Host))
	if isPublicHost(h) {
		return []string{PublicHost, PublicWWWHost, PublicSSHHost}
	}
	return []string{h}
}

func isPublicHost(h string) bool {
	return h == "" || h == PublicHost || h == PublicWWWHost || h == PublicSSHHost
}

// Resolve implements vcs.Provider. GitHub repository paths have exactly
// two segments: owner/repo.
func (p *Provider) Resolve(f vcs.Forge, ref vcs.RepoRef) (vcs.RepoRef, error) {
	segs := ref.Segments()
	if len(segs) != 2 {
		return vcs.RepoRef{}, vcs.Errorf(
			vcs.CodeRepoInvalid,
			f.Name,
			"GitHub repository %q must have exactly 2 path segments (owner/repo), got %d",
			ref.Path, len(segs),
		)
	}
	return vcs.RepoRef{Host: p.Hosts(f)[0], Path: segs[0] + "/" + segs[1]}, nil
}

// Capabilities implements vcs.Provider.
func (p *Provider) Capabilities(vcs.Repo) vcs.Capabilities {
	return vcs.Capabilities{
		ProposalNoun: ProposalNoun,
		Verbs:        append([]vcs.Verb(nil), vcs.ProposalVerbs...),
		Labels:       true,
	}
}

// CloneURL implements vcs.Provider.
func (p *Provider) CloneURL(repo vcs.Repo) string {
	scheme := repo.Forge.Scheme
	if scheme == "" {
		scheme = "https"
	}
	host := p.Hosts(repo.Forge)[0]
	if repo.Forge.Port != 0 && !strings.Contains(host, ":") {
		host = fmt.Sprintf("%s:%d", host, repo.Forge.Port)
	}
	return fmt.Sprintf("%s://%s/%s.git", scheme, host, repo.Ref.Path)
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
	return &vcs.BasicAuth{Username: CloneUsername, Password: tok}, nil
}

// APIBaseURL returns the REST API root URL for f:
// https://api.github.com for github.com, or https://<host[:port]>/api/v3
// for GitHub Enterprise Server.
func (p *Provider) APIBaseURL(f vcs.Forge) string {
	h := strings.ToLower(strings.TrimSpace(f.Host))
	if isPublicHost(h) {
		if p.opts.PublicAPIBaseURL != "" {
			return strings.TrimRight(p.opts.PublicAPIBaseURL, "/")
		}
		if f.Port != 0 {
			scheme := f.Scheme
			if scheme == "" {
				scheme = "https"
			}
			return fmt.Sprintf("%s://api.github.com:%d", scheme, f.Port)
		}
		return PublicAPIBaseURL
	}
	scheme := f.Scheme
	if scheme == "" {
		scheme = "https"
	}
	if f.Port != 0 && !strings.Contains(h, ":") {
		h = fmt.Sprintf("%s:%d", h, f.Port)
	}
	return scheme + "://" + h + EnterpriseAPIPrefix
}

// BaseBranch implements vcs.Provider via GET /repos/{owner}/{repo}.
func (p *Provider) BaseBranch(ctx context.Context, repo vcs.Repo) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	route := "/repos/" + repo.Ref.Path
	if err := p.doJSON(ctx, repo, "", http.MethodGet, route, nil, &out, notFoundAsBaseBranchMissing); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.DefaultBranch) == "" {
		return "", vcs.Errorf(vcs.CodeBaseBranchMissing, repo.Forge.Name, "repository %s reported an empty default_branch", repo.Ref)
	}
	return out.DefaultBranch, nil
}

// ProtectedBranch implements vcs.Provider via
// GET /repos/{owner}/{repo}/branches/{branch}.
//
// If the branch does not yet exist on the remote ("Branch not found"),
// ProtectedBranch returns (false, nil) so a newly created agent branch can
// be published.
func (p *Provider) ProtectedBranch(ctx context.Context, repo vcs.Repo, branch string) (bool, error) {
	var out struct {
		Protected bool `json:"protected"`
	}
	route := "/repos/" + repo.Ref.Path + "/branches/" + url.PathEscape(branch)
	err := p.doJSON(ctx, repo, vcs.VerbPublish, http.MethodGet, route, nil, &out, notFoundOnBranchCheck)
	if err != nil {
		if vcs.IsCode(err, vcs.CodeBaseBranchMissing) {
			return false, nil
		}
		return false, err
	}
	return out.Protected, nil
}

// CreateProposal implements vcs.Provider via POST /repos/{owner}/{repo}/pulls
// (plus POST /repos/{owner}/{repo}/issues/{number}/labels when Labels is non-empty).
func (p *Provider) CreateProposal(ctx context.Context, repo vcs.Repo, req vcs.ProposalRequest) (vcs.Proposal, error) {
	payload := struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body,omitempty"`
		Draft bool   `json:"draft,omitempty"`
	}{
		Title: req.Title,
		Head:  req.Source,
		Base:  req.Target,
		Body:  req.Body,
		Draft: req.Draft,
	}
	var pr ghPull
	route := "/repos/" + repo.Ref.Path + "/pulls"
	if err := p.doJSON(ctx, repo, vcs.VerbProposalCreate, http.MethodPost, route, payload, &pr, notFoundAsUnsupported); err != nil {
		return vcs.Proposal{}, err
	}
	if len(req.Labels) > 0 {
		var lbls []ghLabel
		lblRoute := fmt.Sprintf("/repos/%s/issues/%d/labels", repo.Ref.Path, pr.Number)
		body := struct {
			Labels []string `json:"labels"`
		}{Labels: req.Labels}
		if err := p.doJSON(ctx, repo, vcs.VerbProposalCreate, http.MethodPost, lblRoute, body, &lbls, notFoundAsUnsupported); err != nil {
			return vcs.Proposal{}, err
		}
		pr.Labels = lbls
	}
	return pr.toProposal(), nil
}

// ListProposals implements vcs.Provider via GET /repos/{owner}/{repo}/pulls.
func (p *Provider) ListProposals(ctx context.Context, repo vcs.Repo, f vcs.ProposalFilter) (vcs.ProposalList, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultPerPage
	}
	fetchCount := limit + 1
	if fetchCount > maxPerPage {
		fetchCount = maxPerPage
	}
	pageNum := f.Page.Page
	if pageNum <= 0 {
		pageNum = 1
	}

	q := url.Values{}
	switch f.State {
	case "", vcs.ProposalOpen:
		q.Set("state", "open")
	case vcs.ProposalClosed, vcs.ProposalMerged:
		q.Set("state", "closed")
	default:
		q.Set("state", "all")
	}
	if f.Source != "" {
		owner := repo.Ref.Segments()[0]
		if strings.Contains(f.Source, ":") {
			q.Set("head", f.Source)
		} else {
			q.Set("head", owner+":"+f.Source)
		}
	}
	if f.Target != "" {
		q.Set("base", f.Target)
	}
	q.Set("per_page", strconv.Itoa(fetchCount))
	q.Set("page", strconv.Itoa(pageNum))

	var prs []ghPull
	route := "/repos/" + repo.Ref.Path + "/pulls?" + q.Encode()
	if err := p.doJSON(ctx, repo, vcs.VerbProposalList, http.MethodGet, route, nil, &prs, notFoundAsUnsupported); err != nil {
		return vcs.ProposalList{}, err
	}

	var matched []vcs.Proposal
	for _, pr := range prs {
		prop := pr.toProposal()
		if f.State != "" && prop.State != f.State {
			continue
		}
		if f.Source != "" && prop.Source != f.Source {
			continue
		}
		if f.Target != "" && prop.Target != f.Target {
			continue
		}
		if !hasAllLabels(prop.Labels, f.Labels) {
			continue
		}
		matched = append(matched, prop)
	}
	truncated := false
	if len(matched) > limit {
		matched = matched[:limit]
		truncated = true
	}
	return vcs.ProposalList{Proposals: matched, Truncated: truncated}, nil
}

// GetProposal implements vcs.Provider via GET /repos/{owner}/{repo}/pulls/{number}.
func (p *Provider) GetProposal(ctx context.Context, repo vcs.Repo, number int) (vcs.Proposal, error) {
	var pr ghPull
	route := fmt.Sprintf("/repos/%s/pulls/%d", repo.Ref.Path, number)
	if err := p.doJSON(ctx, repo, vcs.VerbProposalView, http.MethodGet, route, nil, &pr, notFoundAsUnsupported); err != nil {
		return vcs.Proposal{}, err
	}
	return pr.toProposal(), nil
}

// ProposalComments implements vcs.Provider by combining issue comments
// (GET /repos/{owner}/{repo}/issues/{number}/comments), pull request review
// summaries (GET /repos/{owner}/{repo}/pulls/{number}/reviews) and inline
// review comments (GET /repos/{owner}/{repo}/pulls/{number}/comments).
func (p *Provider) ProposalComments(ctx context.Context, repo vcs.Repo, number int, page vcs.Page) (vcs.CommentList, error) {
	limit := page.Limit
	if limit <= 0 {
		limit = defaultPerPage
	}
	pageNum := page.Page
	if pageNum <= 0 {
		pageNum = 1
	}
	q := fmt.Sprintf("?per_page=%d&page=%d", min(limit+1, maxPerPage), pageNum)

	var issueComments []ghIssueComment
	issueRoute := fmt.Sprintf("/repos/%s/issues/%d/comments%s", repo.Ref.Path, number, q)
	if err := p.doJSON(ctx, repo, vcs.VerbProposalComments, http.MethodGet, issueRoute, nil, &issueComments, notFoundAsUnsupported); err != nil {
		return vcs.CommentList{}, err
	}

	var reviews []ghReview
	reviewsRoute := fmt.Sprintf("/repos/%s/pulls/%d/reviews%s", repo.Ref.Path, number, q)
	if err := p.doJSON(ctx, repo, vcs.VerbProposalComments, http.MethodGet, reviewsRoute, nil, &reviews, notFoundAsUnsupported); err != nil {
		return vcs.CommentList{}, err
	}

	var reviewComments []ghReviewComment
	reviewRoute := fmt.Sprintf("/repos/%s/pulls/%d/comments%s", repo.Ref.Path, number, q)
	if err := p.doJSON(ctx, repo, vcs.VerbProposalComments, http.MethodGet, reviewRoute, nil, &reviewComments, notFoundAsUnsupported); err != nil {
		return vcs.CommentList{}, err
	}

	out := make([]vcs.Comment, 0, len(issueComments)+len(reviews)+len(reviewComments))
	for _, ic := range issueComments {
		out = append(out, ic.toComment())
	}
	for _, rv := range reviews {
		if strings.TrimSpace(rv.Body) != "" {
			out = append(out, rv.toComment())
		}
	}
	for _, rc := range reviewComments {
		out = append(out, rc.toComment())
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	truncated := false
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	return vcs.CommentList{Comments: out, Truncated: truncated}, nil
}

// ProposalCommits implements vcs.Provider via
// GET /repos/{owner}/{repo}/pulls/{number}/commits.
func (p *Provider) ProposalCommits(ctx context.Context, repo vcs.Repo, number int, page vcs.Page) (vcs.CommitList, error) {
	limit := page.Limit
	if limit <= 0 {
		limit = defaultPerPage
	}
	pageNum := page.Page
	if pageNum <= 0 {
		pageNum = 1
	}
	route := fmt.Sprintf("/repos/%s/pulls/%d/commits?per_page=%d&page=%d", repo.Ref.Path, number, min(limit+1, maxPerPage), pageNum)
	var raw []ghCommit
	if err := p.doJSON(ctx, repo, vcs.VerbProposalCommits, http.MethodGet, route, nil, &raw, notFoundAsUnsupported); err != nil {
		return vcs.CommitList{}, err
	}
	truncated := false
	if len(raw) > limit {
		raw = raw[:limit]
		truncated = true
	}
	commits := make([]vcs.Commit, 0, len(raw))
	for _, c := range raw {
		commits = append(commits, vcs.Commit{
			Revision: c.SHA,
			Message:  c.Commit.Message,
			Author: vcs.Identity{
				Name:  c.Commit.Author.Name,
				Email: c.Commit.Author.Email,
			},
			When: c.Commit.Author.Date,
		})
	}
	return vcs.CommitList{Commits: commits, Truncated: truncated}, nil
}

// CommentProposal implements vcs.Provider via
// POST /repos/{owner}/{repo}/issues/{number}/comments.
func (p *Provider) CommentProposal(ctx context.Context, repo vcs.Repo, number int, body string) (vcs.Comment, error) {
	route := fmt.Sprintf("/repos/%s/issues/%d/comments", repo.Ref.Path, number)
	in := struct {
		Body string `json:"body"`
	}{Body: body}
	var out ghIssueComment
	if err := p.doJSON(ctx, repo, vcs.VerbProposalComment, http.MethodPost, route, in, &out, notFoundAsUnsupported); err != nil {
		return vcs.Comment{}, err
	}
	return out.toComment(), nil
}

// UpdateProposal implements vcs.Provider via
// PATCH /repos/{owner}/{repo}/pulls/{number} and label endpoints.
func (p *Provider) UpdateProposal(ctx context.Context, repo vcs.Repo, number int, u vcs.ProposalUpdate) (vcs.Proposal, error) {
	for _, add := range u.AddLabels {
		lblRoute := fmt.Sprintf("/repos/%s/issues/%d/labels", repo.Ref.Path, number)
		body := struct {
			Labels []string `json:"labels"`
		}{Labels: []string{add}}
		if err := p.doJSON(ctx, repo, vcs.VerbProposalUpdate, http.MethodPost, lblRoute, body, nil, notFoundAsUnsupported); err != nil {
			return vcs.Proposal{}, err
		}
	}
	for _, rm := range u.RemoveLabels {
		lblRoute := fmt.Sprintf("/repos/%s/issues/%d/labels/%s", repo.Ref.Path, number, url.PathEscape(rm))
		if err := p.doJSON(ctx, repo, vcs.VerbProposalUpdate, http.MethodDelete, lblRoute, nil, nil, notFoundAsUnsupported); err != nil {
			return vcs.Proposal{}, err
		}
	}

	patch := map[string]any{}
	if u.Title != nil {
		patch["title"] = *u.Title
	}
	if u.Body != nil {
		patch["body"] = *u.Body
	}
	if u.Draft != nil {
		patch["draft"] = *u.Draft
	}
	route := fmt.Sprintf("/repos/%s/pulls/%d", repo.Ref.Path, number)
	var pr ghPull
	method := http.MethodPatch
	var in any = patch
	if len(patch) == 0 {
		method = http.MethodGet
		in = nil
	}
	if err := p.doJSON(ctx, repo, vcs.VerbProposalUpdate, method, route, in, &pr, notFoundAsUnsupported); err != nil {
		return vcs.Proposal{}, err
	}
	return pr.toProposal(), nil
}

// CloseProposal implements vcs.Provider via
// PATCH /repos/{owner}/{repo}/pulls/{number} with state="closed".
func (p *Provider) CloseProposal(ctx context.Context, repo vcs.Repo, number int) (vcs.Proposal, error) {
	route := fmt.Sprintf("/repos/%s/pulls/%d", repo.Ref.Path, number)
	in := struct {
		State string `json:"state"`
	}{State: "closed"}
	var pr ghPull
	if err := p.doJSON(ctx, repo, vcs.VerbProposalClose, http.MethodPatch, route, in, &pr, notFoundAsUnsupported); err != nil {
		return vcs.Proposal{}, err
	}
	return pr.toProposal(), nil
}

type notFoundMode int

const (
	notFoundAsUnsupported notFoundMode = iota
	notFoundAsBaseBranchMissing
	notFoundOnBranchCheck
)

func (p *Provider) doJSON(ctx context.Context, repo vcs.Repo, verb vcs.Verb, method, route string, in, out any, nfMode notFoundMode) error {
	client, err := vcs.NewHTTPClient(repo.Forge)
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return vcs.Errorf(vcs.CodeForgeCallFailed, repo.Forge.Name, "marshal request: %w", err).WithVerb(verb)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.APIBaseURL(repo.Forge)+route, body)
	if err != nil {
		return vcs.Errorf(vcs.CodeForgeCallFailed, repo.Forge.Name, "%w", err).WithVerb(verb)
	}
	req.Header.Set("Accept", AcceptHeader)
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if repo.Forge.Cred != nil {
		tok, err := repo.Forge.Cred.Token(ctx)
		if err != nil {
			code := vcs.CodeOf(err)
			if code == "" {
				code = vcs.CodeConfigInvalid
			}
			return vcs.Errorf(code, repo.Forge.Name, "%w", err).WithVerb(verb)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return vcs.TransportError(repo.Forge, verb, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return vcs.TransportError(repo.Forge, verb, err)
	}
	if resp.StatusCode >= 300 {
		return classifyError(repo.Forge.Name, verb, resp, raw, nfMode)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return vcs.Errorf(vcs.CodeForgeCallFailed, repo.Forge.Name, "decode response: %w", err).WithVerb(verb)
		}
	}
	return nil
}

type ghErrorPayload struct {
	Message string `json:"message"`
	Errors  []struct {
		Resource string `json:"resource"`
		Field    string `json:"field"`
		Code     string `json:"code"`
		Message  string `json:"message"`
	} `json:"errors"`
}

func (e ghErrorPayload) format(status int) string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = http.StatusText(status)
	}
	if len(e.Errors) == 0 {
		return msg
	}
	parts := make([]string, 0, len(e.Errors))
	for _, item := range e.Errors {
		var segs []string
		if item.Resource != "" {
			segs = append(segs, item.Resource)
		}
		if item.Field != "" {
			segs = append(segs, item.Field)
		}
		if item.Code != "" {
			segs = append(segs, item.Code)
		}
		if item.Message != "" {
			segs = append(segs, item.Message)
		}
		if len(segs) > 0 {
			parts = append(parts, strings.Join(segs, ": "))
		}
	}
	if len(parts) == 0 {
		return msg
	}
	return fmt.Sprintf("%s (%s)", msg, strings.Join(parts, "; "))
}

func classifyError(forge string, verb vcs.Verb, resp *http.Response, body []byte, nfMode notFoundMode) error {
	var payload ghErrorPayload
	_ = json.Unmarshal(body, &payload)
	detail := payload.format(resp.StatusCode)

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return vcs.Errorf(vcs.CodeForgeUnauthorized, forge, "%s; check the GitHub token, do not retry", detail).WithVerb(verb)

	case http.StatusForbidden:
		if reset := rateLimitResetHint(resp, payload.Message); reset != "" {
			return vcs.Errorf(vcs.CodeForgeUnavailable, forge, "rate limited (%s): %s; wait before retrying", reset, detail).WithVerb(verb)
		}
		return vcs.Errorf(vcs.CodeForgeUnauthorized, forge, "%s; token lacks required permission, do not retry", detail).WithVerb(verb)

	case http.StatusNotFound:
		switch nfMode {
		case notFoundAsBaseBranchMissing:
			return vcs.Errorf(vcs.CodeBaseBranchMissing, forge, "%s; verify the repository exists and is visible to the token", detail).WithVerb(verb)
		case notFoundOnBranchCheck:
			if strings.EqualFold(strings.TrimSpace(payload.Message), "Branch not found") {
				return vcs.Errorf(vcs.CodeBaseBranchMissing, forge, "%s", detail).WithVerb(verb)
			}
			return vcs.Errorf(vcs.CodeForgeUnsupported, forge, "%s", detail).WithVerb(verb)
		default:
			return vcs.Errorf(vcs.CodeForgeUnsupported, forge, "%s", detail).WithVerb(verb)
		}

	case http.StatusUnprocessableEntity:
		return vcs.Errorf(vcs.CodeForgeCallFailed, forge, "%s", detail).WithVerb(verb)

	case http.StatusBadRequest, http.StatusConflict:
		return vcs.Errorf(vcs.CodeForgeRejected, forge, "%s; fix the request fields", detail).WithVerb(verb)

	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		if reset := rateLimitResetHint(resp, payload.Message); reset != "" {
			return vcs.Errorf(vcs.CodeForgeUnavailable, forge, "%s (%s); retry later", detail, reset).WithVerb(verb)
		}
		return vcs.Errorf(vcs.CodeForgeUnavailable, forge, "%s; retry the call", detail).WithVerb(verb)

	default:
		return vcs.Errorf(vcs.CodeForgeCallFailed, forge, "HTTP %d: %s", resp.StatusCode, detail).WithVerb(verb)
	}
}

func rateLimitResetHint(resp *http.Response, msg string) string {
	if retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After")); retryAfter != "" {
		return "retry-after " + retryAfter
	}
	rem := strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining"))
	reset := strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset"))
	if reset != "" && (rem == "" || rem == "0") {
		if sec, err := strconv.ParseInt(reset, 10, 64); err == nil {
			return "reset at " + time.Unix(sec, 0).UTC().Format(time.RFC3339) + " (" + reset + ")"
		}
		return "reset at " + reset
	}
	if strings.Contains(strings.ToLower(msg), "rate limit") {
		return "rate limit exceeded"
	}
	return ""
}

type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

func (u ghUser) toActor() vcs.Actor {
	login := strings.TrimSuffix(u.Login, "[bot]")
	return vcs.Actor{
		Login: login,
		Bot:   strings.EqualFold(u.Type, "Bot") || strings.HasSuffix(u.Login, "[bot]"),
	}
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghPull struct {
	Number    int       `json:"number"`
	HTMLURL   string    `json:"html_url"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Draft     bool      `json:"draft"`
	Merged    bool      `json:"merged"`
	MergedAt  *string   `json:"merged_at"`
	User      ghUser    `json:"user"`
	Labels    []ghLabel `json:"labels"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Head      struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (pr ghPull) toProposal() vcs.Proposal {
	state := vcs.ProposalOpen
	switch {
	case pr.Merged || (pr.MergedAt != nil && *pr.MergedAt != ""):
		state = vcs.ProposalMerged
	case strings.EqualFold(pr.State, "closed"):
		state = vcs.ProposalClosed
	}
	labels := make([]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		if l.Name != "" {
			labels = append(labels, l.Name)
		}
	}
	return vcs.Proposal{
		Number:    pr.Number,
		URL:       pr.HTMLURL,
		Title:     pr.Title,
		Body:      pr.Body,
		State:     state,
		Source:    pr.Head.Ref,
		Target:    pr.Base.Ref,
		Revision:  pr.Head.SHA,
		Draft:     pr.Draft,
		Author:    pr.User.toActor(),
		Labels:    labels,
		CreatedAt: pr.CreatedAt,
		UpdatedAt: pr.UpdatedAt,
	}
}

type ghIssueComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	User      ghUser    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

func (c ghIssueComment) toComment() vcs.Comment {
	return vcs.Comment{
		ID:        strconv.FormatInt(c.ID, 10),
		Kind:      vcs.CommentDiscussion,
		Author:    c.User.toActor(),
		Body:      c.Body,
		URL:       c.HTMLURL,
		CreatedAt: c.CreatedAt,
	}
}

type ghReview struct {
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	User        ghUser    `json:"user"`
	SubmittedAt time.Time `json:"submitted_at"`
}

func (r ghReview) toComment() vcs.Comment {
	return vcs.Comment{
		ID:        strconv.FormatInt(r.ID, 10),
		Kind:      vcs.CommentReview,
		Author:    r.User.toActor(),
		Body:      r.Body,
		URL:       r.HTMLURL,
		CreatedAt: r.SubmittedAt,
	}
}

type ghReviewComment struct {
	ID           int64     `json:"id"`
	Body         string    `json:"body"`
	Path         string    `json:"path"`
	Line         int       `json:"line"`
	OriginalLine int       `json:"original_line"`
	HTMLURL      string    `json:"html_url"`
	User         ghUser    `json:"user"`
	CreatedAt    time.Time `json:"created_at"`
}

func (c ghReviewComment) toComment() vcs.Comment {
	line := c.Line
	if line == 0 {
		line = c.OriginalLine
	}
	return vcs.Comment{
		ID:        strconv.FormatInt(c.ID, 10),
		Kind:      vcs.CommentReviewComment,
		Author:    c.User.toActor(),
		Body:      c.Body,
		Path:      c.Path,
		Line:      line,
		URL:       c.HTMLURL,
		CreatedAt: c.CreatedAt,
	}
}

type ghCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name  string    `json:"name"`
			Email string    `json:"email"`
			Date  time.Time `json:"date"`
		} `json:"author"`
	} `json:"commit"`
}

func hasAllLabels(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if strings.EqualFold(h, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
