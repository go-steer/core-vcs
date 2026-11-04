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

// Package gitlab implements vcs.Provider for gitlab.com and self-managed
// GitLab instances over net/http with no third-party SDK.
//
// All GitLab-specific host names, API base URL rules (/api/v4), project
// path encoding, PRIVATE-TOKEN headers and oauth2 clone basic-auth
// conventions live inside this package.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-steer/core-vcs/vcs"
)

const (
	// Name is the registry name of the GitLab provider.
	Name = "gitlab"
	// PublicHost is the canonical public GitLab host.
	PublicHost = "gitlab.com"
	// PublicWWWHost is the www alias of PublicHost.
	PublicWWWHost = "www.gitlab.com"
	// APIPrefix is the REST API v4 path prefix on both gitlab.com and
	// self-managed GitLab.
	APIPrefix = "/api/v4"
	// PrivateTokenHeader is the HTTP request header GitLab uses for
	// personal, project and group access tokens.
	PrivateTokenHeader = "PRIVATE-TOKEN"
	// CloneUsername is the HTTP Basic Auth username GitLab requires when
	// authenticating smart-HTTP git operations with a token.
	CloneUsername = "oauth2"
	// ProposalNoun is what GitLab calls a proposal.
	ProposalNoun = "merge request"

	defaultPerPage   = 30
	maxPerPage       = 100
	maxResponseBytes = 16 << 20 // 16 MiB ceiling on API response bodies
)

// Options configures a Provider instance for testing. Production callers
// use New.
type Options struct {
	// PublicAPIBaseURL overrides the https://gitlab.com/api/v4 base URL when
	// Forge.Host targets gitlab.com, allowing contract tests to point
	// gitlab.com API traffic at an httptest server without changing
	// Forge.Host.
	PublicAPIBaseURL string
}

// Provider is the GitLab vcs.Provider implementation.
type Provider struct {
	opts Options
}

// New constructs a GitLab Provider with default production settings.
func New() (vcs.Provider, error) {
	return &Provider{}, nil
}

// NewWithOptions constructs a GitLab Provider with opts.
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
	if isPublicHost(h) {
		if f.CAFile != "" {
			return vcs.Errorf(vcs.CodeConfigInvalid, f.Name, "CAFile is only valid when Host names a self-managed GitLab instance, not %s", PublicHost)
		}
		if f.Scheme == "http" && p.opts.PublicAPIBaseURL == "" {
			return vcs.Errorf(vcs.CodeConfigInvalid, f.Name, "http scheme is only valid on an in-cluster self-managed GitLab instance, not %s", PublicHost)
		}
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
		return []string{PublicHost, PublicWWWHost}
	}
	return []string{h}
}

func isPublicHost(h string) bool {
	return h == "" || h == PublicHost || h == PublicWWWHost
}

// Resolve implements vcs.Provider. GitLab project paths have two or more
// segments (group/project, group/subgroup/project, ...).
func (p *Provider) Resolve(f vcs.Forge, ref vcs.RepoRef) (vcs.RepoRef, error) {
	segs := ref.Segments()
	if len(segs) < 2 {
		return vcs.RepoRef{}, vcs.Errorf(
			vcs.CodeRepoInvalid,
			f.Name,
			"GitLab repository %q must have at least 2 path segments (group/project), got %d",
			ref.Path, len(segs),
		)
	}
	return vcs.RepoRef{Host: p.Hosts(f)[0], Path: strings.Join(segs, "/")}, nil
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

// APIBaseURL returns the REST API v4 root URL for f:
// https://gitlab.com/api/v4 for gitlab.com, or <scheme>://<host[:port]>/api/v4
// for self-managed GitLab.
func (p *Provider) APIBaseURL(f vcs.Forge) string {
	h := strings.ToLower(strings.TrimSpace(f.Host))
	if isPublicHost(h) && p.opts.PublicAPIBaseURL != "" {
		return strings.TrimRight(p.opts.PublicAPIBaseURL, "/")
	}
	scheme := f.Scheme
	if scheme == "" {
		scheme = "https"
	}
	host := p.Hosts(f)[0]
	if f.Port != 0 && !strings.Contains(host, ":") {
		host = fmt.Sprintf("%s:%d", host, f.Port)
	}
	return scheme + "://" + host + APIPrefix
}

func projectRoute(repo vcs.Repo, suffix string) string {
	return "/projects/" + url.PathEscape(repo.Ref.Path) + suffix
}

// BaseBranch implements vcs.Provider via GET /projects/{id}.
func (p *Provider) BaseBranch(ctx context.Context, repo vcs.Repo) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := p.doJSON(ctx, repo, "", http.MethodGet, projectRoute(repo, ""), nil, &out, notFoundAsBaseBranchMissing); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.DefaultBranch) == "" {
		return "", vcs.Errorf(vcs.CodeBaseBranchMissing, repo.Forge.Name, "project %s reported an empty default_branch", repo.Ref)
	}
	return out.DefaultBranch, nil
}

// ProtectedBranch implements vcs.Provider via
// GET /projects/{id}/protected_branches.
func (p *Provider) ProtectedBranch(ctx context.Context, repo vcs.Repo, branch string) (bool, error) {
	var rules []struct {
		Name string `json:"name"`
	}
	if err := p.doJSON(ctx, repo, vcs.VerbPublish, http.MethodGet, projectRoute(repo, "/protected_branches"), nil, &rules, notFoundAsUnsupported); err != nil {
		return false, err
	}
	for _, r := range rules {
		if gitlabWildcardMatch(r.Name, branch) {
			return true, nil
		}
	}
	return false, nil
}

// gitlabWildcardMatch implements GitLab's protected-branch wildcard rule
// where '*' matches any sequence of characters including '/'.
func gitlabWildcardMatch(pattern, branch string) bool {
	if pattern == branch {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return false
	}
	parts := strings.Split(pattern, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	re, err := regexp.Compile("^" + strings.Join(parts, ".*") + "$")
	if err != nil {
		return false
	}
	return re.MatchString(branch)
}

// CreateProposal implements vcs.Provider via
// POST /projects/{id}/merge_requests.
func (p *Provider) CreateProposal(ctx context.Context, repo vcs.Repo, req vcs.ProposalRequest) (vcs.Proposal, error) {
	title := req.Title
	if req.Draft && !hasDraftPrefix(title) {
		title = "Draft: " + title
	}
	payload := struct {
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		Title        string `json:"title"`
		Description  string `json:"description,omitempty"`
		Labels       string `json:"labels,omitempty"`
	}{
		SourceBranch: req.Source,
		TargetBranch: req.Target,
		Title:        title,
		Description:  req.Body,
		Labels:       strings.Join(req.Labels, ","),
	}
	var mr glMergeRequest
	if err := p.doJSON(ctx, repo, vcs.VerbProposalCreate, http.MethodPost, projectRoute(repo, "/merge_requests"), payload, &mr, notFoundAsUnsupported); err != nil {
		return vcs.Proposal{}, err
	}
	return mr.toProposal(), nil
}

// ListProposals implements vcs.Provider via
// GET /projects/{id}/merge_requests.
func (p *Provider) ListProposals(ctx context.Context, repo vcs.Repo, f vcs.ProposalFilter) (vcs.ProposalList, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultPerPage
	}
	fetchCount := min(limit+1, maxPerPage)
	pageNum := f.Page.Page
	if pageNum <= 0 {
		pageNum = 1
	}

	q := url.Values{}
	switch f.State {
	case "", vcs.ProposalOpen:
		q.Set("state", "opened")
	case vcs.ProposalClosed:
		q.Set("state", "closed")
	case vcs.ProposalMerged:
		q.Set("state", "merged")
	default:
		q.Set("state", "all")
	}
	if f.Source != "" {
		q.Set("source_branch", f.Source)
	}
	if f.Target != "" {
		q.Set("target_branch", f.Target)
	}
	if len(f.Labels) > 0 {
		q.Set("labels", strings.Join(f.Labels, ","))
	}
	q.Set("per_page", strconv.Itoa(fetchCount))
	q.Set("page", strconv.Itoa(pageNum))

	var mrs []glMergeRequest
	route := projectRoute(repo, "/merge_requests?"+q.Encode())
	if err := p.doJSON(ctx, repo, vcs.VerbProposalList, http.MethodGet, route, nil, &mrs, notFoundAsUnsupported); err != nil {
		return vcs.ProposalList{}, err
	}

	var matched []vcs.Proposal
	for _, mr := range mrs {
		prop := mr.toProposal()
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

// GetProposal implements vcs.Provider via
// GET /projects/{id}/merge_requests/{iid}.
func (p *Provider) GetProposal(ctx context.Context, repo vcs.Repo, number int) (vcs.Proposal, error) {
	var mr glMergeRequest
	route := projectRoute(repo, fmt.Sprintf("/merge_requests/%d", number))
	if err := p.doJSON(ctx, repo, vcs.VerbProposalView, http.MethodGet, route, nil, &mr, notFoundAsUnsupported); err != nil {
		return vcs.Proposal{}, err
	}
	return mr.toProposal(), nil
}

// ProposalComments implements vcs.Provider via
// GET /projects/{id}/merge_requests/{iid}/notes.
func (p *Provider) ProposalComments(ctx context.Context, repo vcs.Repo, number int, page vcs.Page) (vcs.CommentList, error) {
	limit := page.Limit
	if limit <= 0 {
		limit = defaultPerPage
	}
	pageNum := page.Page
	if pageNum <= 0 {
		pageNum = 1
	}
	fetchCount := min(max(limit*2, defaultPerPage), maxPerPage)
	route := projectRoute(repo, fmt.Sprintf("/merge_requests/%d/notes?per_page=%d&page=%d", number, fetchCount, pageNum))
	var notes []glNote
	if err := p.doJSON(ctx, repo, vcs.VerbProposalComments, http.MethodGet, route, nil, &notes, notFoundAsUnsupported); err != nil {
		return vcs.CommentList{}, err
	}

	var out []vcs.Comment
	for _, n := range notes {
		if n.System {
			continue
		}
		out = append(out, n.toComment(repo, number))
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
// GET /projects/{id}/merge_requests/{iid}/commits.
func (p *Provider) ProposalCommits(ctx context.Context, repo vcs.Repo, number int, page vcs.Page) (vcs.CommitList, error) {
	limit := page.Limit
	if limit <= 0 {
		limit = defaultPerPage
	}
	pageNum := page.Page
	if pageNum <= 0 {
		pageNum = 1
	}
	route := projectRoute(repo, fmt.Sprintf("/merge_requests/%d/commits?per_page=%d&page=%d", number, min(limit+1, maxPerPage), pageNum))
	var raw []glCommit
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
		msg := c.Message
		if msg == "" {
			msg = c.Title
		}
		when := c.AuthoredDate
		if when.IsZero() {
			when = c.CreatedAt
		}
		commits = append(commits, vcs.Commit{
			Revision: c.ID,
			Message:  msg,
			Author: vcs.Identity{
				Name:  c.AuthorName,
				Email: c.AuthorEmail,
			},
			When: when,
		})
	}
	return vcs.CommitList{Commits: commits, Truncated: truncated}, nil
}

// CommentProposal implements vcs.Provider via
// POST /projects/{id}/merge_requests/{iid}/notes.
func (p *Provider) CommentProposal(ctx context.Context, repo vcs.Repo, number int, body string) (vcs.Comment, error) {
	route := projectRoute(repo, fmt.Sprintf("/merge_requests/%d/notes", number))
	in := struct {
		Body string `json:"body"`
	}{Body: body}
	var n glNote
	if err := p.doJSON(ctx, repo, vcs.VerbProposalComment, http.MethodPost, route, in, &n, notFoundAsUnsupported); err != nil {
		return vcs.Comment{}, err
	}
	return n.toComment(repo, number), nil
}

// UpdateProposal implements vcs.Provider via
// PUT /projects/{id}/merge_requests/{iid}.
func (p *Provider) UpdateProposal(ctx context.Context, repo vcs.Repo, number int, u vcs.ProposalUpdate) (vcs.Proposal, error) {
	route := projectRoute(repo, fmt.Sprintf("/merge_requests/%d", number))

	var titlePtr *string
	if u.Title != nil {
		t := *u.Title
		if u.Draft != nil {
			if *u.Draft && !hasDraftPrefix(t) {
				t = "Draft: " + t
			} else if !*u.Draft {
				t = stripDraftPrefix(t)
			}
		}
		titlePtr = &t
	} else if u.Draft != nil {
		cur, err := p.GetProposal(ctx, repo, number)
		if err != nil {
			return vcs.Proposal{}, err
		}
		t := cur.Title
		if *u.Draft && !hasDraftPrefix(t) {
			t = "Draft: " + t
		} else if !*u.Draft {
			t = stripDraftPrefix(t)
		}
		titlePtr = &t
	}

	payload := map[string]any{}
	if titlePtr != nil {
		payload["title"] = *titlePtr
	}
	if u.Body != nil {
		payload["description"] = *u.Body
	}
	if len(u.AddLabels) > 0 {
		payload["add_labels"] = strings.Join(u.AddLabels, ",")
	}
	if len(u.RemoveLabels) > 0 {
		payload["remove_labels"] = strings.Join(u.RemoveLabels, ",")
	}

	var mr glMergeRequest
	method := http.MethodPut
	var in any = payload
	if len(payload) == 0 {
		method = http.MethodGet
		in = nil
	}
	if err := p.doJSON(ctx, repo, vcs.VerbProposalUpdate, method, route, in, &mr, notFoundAsUnsupported); err != nil {
		return vcs.Proposal{}, err
	}
	return mr.toProposal(), nil
}

// CloseProposal implements vcs.Provider via
// PUT /projects/{id}/merge_requests/{iid} with state_event="close".
func (p *Provider) CloseProposal(ctx context.Context, repo vcs.Repo, number int) (vcs.Proposal, error) {
	route := projectRoute(repo, fmt.Sprintf("/merge_requests/%d", number))
	in := struct {
		StateEvent string `json:"state_event"`
	}{StateEvent: "close"}
	var mr glMergeRequest
	if err := p.doJSON(ctx, repo, vcs.VerbProposalClose, http.MethodPut, route, in, &mr, notFoundAsUnsupported); err != nil {
		return vcs.Proposal{}, err
	}
	return mr.toProposal(), nil
}

type notFoundMode int

const (
	notFoundAsUnsupported notFoundMode = iota
	notFoundAsBaseBranchMissing
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
		req.Header.Set(PrivateTokenHeader, tok)
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

type glErrorPayload struct {
	Message          json.RawMessage `json:"message"`
	Error            string          `json:"error"`
	ErrorDescription string          `json:"error_description"`
}

func (e glErrorPayload) format(status int) string {
	var parts []string
	if len(e.Message) > 0 {
		var s string
		if err := json.Unmarshal(e.Message, &s); err == nil {
			if strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimSpace(s))
			}
		} else {
			raw := strings.TrimSpace(string(e.Message))
			if raw != "" && raw != "null" {
				parts = append(parts, raw)
			}
		}
	}
	if strings.TrimSpace(e.Error) != "" {
		parts = append(parts, strings.TrimSpace(e.Error))
	}
	if strings.TrimSpace(e.ErrorDescription) != "" {
		parts = append(parts, strings.TrimSpace(e.ErrorDescription))
	}
	if len(parts) == 0 {
		return http.StatusText(status)
	}
	return strings.Join(parts, ": ")
}

func classifyError(forge string, verb vcs.Verb, resp *http.Response, body []byte, nfMode notFoundMode) error {
	var payload glErrorPayload
	_ = json.Unmarshal(body, &payload)
	detail := payload.format(resp.StatusCode)

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return vcs.Errorf(vcs.CodeForgeUnauthorized, forge, "%s; check the GitLab token, do not retry", detail).WithVerb(verb)

	case http.StatusForbidden:
		if reset := rateLimitResetHint(resp, detail); reset != "" {
			return vcs.Errorf(vcs.CodeForgeUnavailable, forge, "rate limited (%s): %s; wait before retrying", reset, detail).WithVerb(verb)
		}
		return vcs.Errorf(vcs.CodeForgeUnauthorized, forge, "%s; token lacks required permission, do not retry", detail).WithVerb(verb)

	case http.StatusNotFound:
		if nfMode == notFoundAsBaseBranchMissing {
			return vcs.Errorf(vcs.CodeBaseBranchMissing, forge, "%s; verify the project exists and is visible to the token", detail).WithVerb(verb)
		}
		return vcs.Errorf(vcs.CodeForgeUnsupported, forge, "%s", detail).WithVerb(verb)

	case http.StatusUnprocessableEntity:
		return vcs.Errorf(vcs.CodeForgeCallFailed, forge, "%s", detail).WithVerb(verb)

	case http.StatusBadRequest, http.StatusConflict:
		return vcs.Errorf(vcs.CodeForgeRejected, forge, "%s; fix the request fields", detail).WithVerb(verb)

	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		if reset := rateLimitResetHint(resp, detail); reset != "" {
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
	for _, hdr := range []string{"RateLimit-Reset", "X-RateLimit-Reset"} {
		if reset := strings.TrimSpace(resp.Header.Get(hdr)); reset != "" {
			if sec, err := strconv.ParseInt(reset, 10, 64); err == nil {
				return "reset at " + time.Unix(sec, 0).UTC().Format(time.RFC3339) + " (" + reset + ")"
			}
			return "reset at " + reset
		}
	}
	if strings.Contains(strings.ToLower(msg), "rate limit") || strings.Contains(strings.ToLower(msg), "throttle") {
		return "rate limit exceeded"
	}
	return ""
}

type glUser struct {
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

func (u glUser) toActor() vcs.Actor {
	login := strings.TrimSuffix(u.Username, "[bot]")
	return vcs.Actor{
		Login: login,
		Bot:   u.Bot || strings.HasSuffix(u.Username, "[bot]"),
	}
}

type glMergeRequest struct {
	IID            int       `json:"iid"`
	WebURL         string    `json:"web_url"`
	Title          string    `json:"title"`
	Description    string    `json:"description"`
	State          string    `json:"state"`
	Draft          bool      `json:"draft"`
	WorkInProgress bool      `json:"work_in_progress"`
	SourceBranch   string    `json:"source_branch"`
	TargetBranch   string    `json:"target_branch"`
	SHA            string    `json:"sha"`
	Author         glUser    `json:"author"`
	Labels         []string  `json:"labels"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (mr glMergeRequest) toProposal() vcs.Proposal {
	state := vcs.ProposalOpen
	switch strings.ToLower(mr.State) {
	case "merged":
		state = vcs.ProposalMerged
	case "closed":
		state = vcs.ProposalClosed
	}
	draft := mr.Draft || mr.WorkInProgress || hasDraftPrefix(mr.Title)
	return vcs.Proposal{
		Number:    mr.IID,
		URL:       mr.WebURL,
		Title:     stripDraftPrefix(mr.Title),
		Body:      mr.Description,
		State:     state,
		Source:    mr.SourceBranch,
		Target:    mr.TargetBranch,
		Revision:  mr.SHA,
		Draft:     draft,
		Author:    mr.Author.toActor(),
		Labels:    append([]string(nil), mr.Labels...),
		CreatedAt: mr.CreatedAt,
		UpdatedAt: mr.UpdatedAt,
	}
}

func hasDraftPrefix(title string) bool {
	for _, pfx := range []string{"Draft:", "[Draft]", "(Draft)", "WIP:", "[WIP]"} {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(title)), strings.ToLower(pfx)) {
			return true
		}
	}
	return false
}

func stripDraftPrefix(title string) string {
	t := strings.TrimSpace(title)
	for _, pfx := range []string{"Draft:", "[Draft]", "(Draft)", "WIP:", "[WIP]"} {
		if strings.HasPrefix(strings.ToLower(t), strings.ToLower(pfx)) {
			return strings.TrimSpace(t[len(pfx):])
		}
	}
	return title
}

type glNote struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	System    bool      `json:"system"`
	Author    glUser    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
	Position  *struct {
		NewPath string `json:"new_path"`
		OldPath string `json:"old_path"`
		NewLine int    `json:"new_line"`
		OldLine int    `json:"old_line"`
	} `json:"position"`
}

func (n glNote) toComment(repo vcs.Repo, mrIID int) vcs.Comment {
	scheme := repo.Forge.Scheme
	if scheme == "" {
		scheme = "https"
	}
	urlStr := fmt.Sprintf("%s://%s/%s/-/merge_requests/%d#note_%d", scheme, repo.Ref.Host, repo.Ref.Path, mrIID, n.ID)
	kind := vcs.CommentDiscussion
	var filePath string
	var line int
	if n.Position != nil && (n.Position.NewPath != "" || n.Position.OldPath != "") {
		kind = vcs.CommentReviewComment
		filePath = n.Position.NewPath
		if filePath == "" {
			filePath = n.Position.OldPath
		}
		line = n.Position.NewLine
		if line == 0 {
			line = n.Position.OldLine
		}
	}
	return vcs.Comment{
		ID:        strconv.FormatInt(n.ID, 10),
		Kind:      kind,
		Author:    n.Author.toActor(),
		Body:      n.Body,
		Path:      filePath,
		Line:      line,
		URL:       urlStr,
		CreatedAt: n.CreatedAt,
	}
}

type glCommit struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Message      string    `json:"message"`
	AuthorName   string    `json:"author_name"`
	AuthorEmail  string    `json:"author_email"`
	AuthoredDate time.Time `json:"authored_date"`
	CreatedAt    time.Time `json:"created_at"`
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
