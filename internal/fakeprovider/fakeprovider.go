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

// Package fakeprovider is an httptest-backed vcs.Provider used to prove
// the contract test and the core in isolation from any real forge. Its
// client half uses vcs.NewHTTPClient and vcs.TransportError the way a
// shipped provider does, so TLS, CAFile, credential redaction and status
// translation are exercised end to end.
package fakeprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-vcs/vcs"
)

const (
	// Name is the registry name of the fake provider.
	Name = "fake"
	// PublicHost is the default host when Forge.Host is empty.
	PublicHost = "fake.forge.example"
	// PublicAlias is a secondary spelling of PublicHost.
	PublicAlias = "www.fake.forge.example"
	// ValidToken is the token the fixture server accepts.
	ValidToken = "fake-secret-token-do-not-log"
	// CloneUser is the HTTP basic-auth username CloneAuth returns.
	CloneUser = "x-fake-token"
)

// Options shapes a Provider instance for a test case.
type Options struct {
	Noun      string
	Verbs     []vcs.Verb
	Labels    bool
	DialURL   string          // overrides the scheme://host[:port] the client dials
	CloneURL  string          // overrides CloneURL when non-empty
	Protected map[string]bool // overrides ProtectedBranch when non-nil
}

// Provider is the client half.
type Provider struct {
	opts Options
}

// New returns a Constructor with the default options: all proposal verbs,
// labels enabled, noun "change proposal".
func New(opts Options) vcs.Constructor {
	if opts.Noun == "" {
		opts.Noun = "change proposal"
	}
	if opts.Verbs == nil {
		opts.Verbs = append([]vcs.Verb(nil), vcs.ProposalVerbs...)
	}
	return func() (vcs.Provider, error) {
		return &Provider{opts: opts}, nil
	}
}

// Name implements vcs.Provider.
func (p *Provider) Name() string { return Name }

// ValidateForge implements vcs.Provider.
func (p *Provider) ValidateForge(f vcs.Forge) error {
	if f.Host == "" && f.CAFile != "" {
		return vcs.Errorf(vcs.CodeConfigInvalid, f.Name, "CAFile is only valid when Host names a self-managed instance")
	}
	switch f.Scheme {
	case "", "https", "http":
		return nil
	default:
		return vcs.Errorf(vcs.CodeConfigInvalid, f.Name, "scheme %q is not https or http", f.Scheme)
	}
}

// Hosts implements vcs.Provider.
func (p *Provider) Hosts(f vcs.Forge) []string {
	if f.Host != "" {
		return []string{f.Host}
	}
	return []string{PublicHost, PublicAlias}
}

// Resolve implements vcs.Provider. The fake grammar accepts two or three
// path segments, so both two-segment and nested paths are tested and one-
// or four-segment paths are refused.
func (p *Provider) Resolve(f vcs.Forge, ref vcs.RepoRef) (vcs.RepoRef, error) {
	segs := ref.Segments()
	if len(segs) < 2 || len(segs) > 3 {
		return vcs.RepoRef{}, vcs.Errorf(vcs.CodeRepoInvalid, f.Name, "repository %q must have 2 or 3 path segments on %s, got %d", ref.Path, Name, len(segs))
	}
	return vcs.RepoRef{Host: p.Hosts(f)[0], Path: ref.Path}, nil
}

// Capabilities implements vcs.Provider.
func (p *Provider) Capabilities(vcs.Repo) vcs.Capabilities {
	return vcs.Capabilities{
		ProposalNoun: p.opts.Noun,
		Verbs:        append([]vcs.Verb(nil), p.opts.Verbs...),
		Labels:       p.opts.Labels,
	}
}

// CloneURL implements vcs.Provider.
func (p *Provider) CloneURL(repo vcs.Repo) string {
	if p.opts.CloneURL != "" {
		return p.opts.CloneURL
	}
	return p.baseURL(repo.Forge) + "/" + repo.Ref.Path + ".git"
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
	return &vcs.BasicAuth{Username: "x-fake-token", Password: tok}, nil
}

// BaseBranch implements vcs.Provider.
func (p *Provider) BaseBranch(ctx context.Context, repo vcs.Repo) (string, error) {
	if p.opts.DialURL == "" && p.opts.CloneURL != "" {
		return "main", nil
	}
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := p.call(ctx, repo, "", http.MethodGet, "/api/repos/"+repo.Ref.Path, nil, &out); err != nil {
		return "", err
	}
	if out.DefaultBranch == "" {
		return "", vcs.Errorf(vcs.CodeBaseBranchMissing, repo.Forge.Name, "repository %s has no default branch", repo.Ref)
	}
	return out.DefaultBranch, nil
}

// ProtectedBranch implements vcs.Provider.
func (p *Provider) ProtectedBranch(ctx context.Context, repo vcs.Repo, branch string) (bool, error) {
	if p.opts.Protected != nil {
		return p.opts.Protected[branch], nil
	}
	if p.opts.DialURL == "" && p.opts.CloneURL != "" {
		return branch == "main", nil
	}
	var out struct {
		Protected bool `json:"protected"`
	}
	path := "/api/repos/" + repo.Ref.Path + "/branches/" + url.PathEscape(branch)
	if err := p.call(ctx, repo, vcs.VerbPublish, http.MethodGet, path, nil, &out); err != nil {
		return false, err
	}
	return out.Protected, nil
}

// CreateProposal implements vcs.Provider.
func (p *Provider) CreateProposal(ctx context.Context, repo vcs.Repo, req vcs.ProposalRequest) (vcs.Proposal, error) {
	if p.opts.Noun == vcs.ProposalNounLink {
		return vcs.Proposal{
			URL:    fmt.Sprintf("https://%s/%s/compare/%s...%s", repo.Ref.Host, repo.Ref.Path, req.Target, req.Source),
			Source: req.Source,
			Target: req.Target,
		}, nil
	}
	var out vcs.Proposal
	err := p.call(ctx, repo, vcs.VerbProposalCreate, http.MethodPost, "/api/repos/"+repo.Ref.Path+"/proposals", req, &out)
	return out, err
}

// ListProposals implements vcs.Provider.
func (p *Provider) ListProposals(ctx context.Context, repo vcs.Repo, f vcs.ProposalFilter) (vcs.ProposalList, error) {
	q := url.Values{}
	if f.State != "" {
		q.Set("state", string(f.State))
	}
	if f.Source != "" {
		q.Set("source", f.Source)
	}
	if f.Target != "" {
		q.Set("target", f.Target)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var out vcs.ProposalList
	err := p.call(ctx, repo, vcs.VerbProposalList, http.MethodGet, "/api/repos/"+repo.Ref.Path+"/proposals?"+q.Encode(), nil, &out)
	return out, err
}

// GetProposal implements vcs.Provider.
func (p *Provider) GetProposal(ctx context.Context, repo vcs.Repo, number int) (vcs.Proposal, error) {
	var out vcs.Proposal
	path := fmt.Sprintf("/api/repos/%s/proposals/%d", repo.Ref.Path, number)
	err := p.call(ctx, repo, vcs.VerbProposalView, http.MethodGet, path, nil, &out)
	return out, err
}

// ProposalComments implements vcs.Provider.
func (p *Provider) ProposalComments(ctx context.Context, repo vcs.Repo, number int, page vcs.Page) (vcs.CommentList, error) {
	var out vcs.CommentList
	path := fmt.Sprintf("/api/repos/%s/proposals/%d/comments?limit=%d", repo.Ref.Path, number, page.Limit)
	err := p.call(ctx, repo, vcs.VerbProposalComments, http.MethodGet, path, nil, &out)
	return out, err
}

// ProposalCommits implements vcs.Provider.
func (p *Provider) ProposalCommits(ctx context.Context, repo vcs.Repo, number int, page vcs.Page) (vcs.CommitList, error) {
	var out vcs.CommitList
	path := fmt.Sprintf("/api/repos/%s/proposals/%d/commits?limit=%d", repo.Ref.Path, number, page.Limit)
	err := p.call(ctx, repo, vcs.VerbProposalCommits, http.MethodGet, path, nil, &out)
	return out, err
}

// CommentProposal implements vcs.Provider.
func (p *Provider) CommentProposal(ctx context.Context, repo vcs.Repo, number int, body string) (vcs.Comment, error) {
	var out vcs.Comment
	path := fmt.Sprintf("/api/repos/%s/proposals/%d/comments", repo.Ref.Path, number)
	err := p.call(ctx, repo, vcs.VerbProposalComment, http.MethodPost, path, map[string]string{"body": body}, &out)
	return out, err
}

// UpdateProposal implements vcs.Provider.
func (p *Provider) UpdateProposal(ctx context.Context, repo vcs.Repo, number int, u vcs.ProposalUpdate) (vcs.Proposal, error) {
	var out vcs.Proposal
	path := fmt.Sprintf("/api/repos/%s/proposals/%d", repo.Ref.Path, number)
	err := p.call(ctx, repo, vcs.VerbProposalUpdate, http.MethodPatch, path, u, &out)
	return out, err
}

// CloseProposal implements vcs.Provider.
func (p *Provider) CloseProposal(ctx context.Context, repo vcs.Repo, number int) (vcs.Proposal, error) {
	var out vcs.Proposal
	path := fmt.Sprintf("/api/repos/%s/proposals/%d/close", repo.Ref.Path, number)
	err := p.call(ctx, repo, vcs.VerbProposalClose, http.MethodPost, path, nil, &out)
	return out, err
}

func (p *Provider) baseURL(f vcs.Forge) string {
	if p.opts.DialURL != "" {
		return strings.TrimRight(p.opts.DialURL, "/")
	}
	scheme := f.Scheme
	if scheme == "" {
		scheme = "https"
	}
	host := p.Hosts(f)[0]
	if f.Port != 0 && !strings.Contains(host, ":") {
		host = fmt.Sprintf("%s:%d", host, f.Port)
	}
	return scheme + "://" + host
}

func (p *Provider) call(ctx context.Context, repo vcs.Repo, verb vcs.Verb, method, path string, in, out any) error {
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
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL(repo.Forge)+path, body)
	if err != nil {
		return vcs.Errorf(vcs.CodeForgeCallFailed, repo.Forge.Name, "%w", err).WithVerb(verb)
	}
	if repo.Forge.Cred != nil {
		tok, err := repo.Forge.Cred.Token(ctx)
		if err != nil {
			return vcs.Errorf(vcs.CodeOf(err), repo.Forge.Name, "%w", err).WithVerb(verb)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return vcs.TransportError(repo.Forge, verb, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return vcs.TransportError(repo.Forge, verb, err)
	}
	if resp.StatusCode >= 300 {
		return classifyHTTP(repo.Forge.Name, verb, resp, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return vcs.Errorf(vcs.CodeForgeCallFailed, repo.Forge.Name, "decode response: %w", err).WithVerb(verb)
		}
	}
	return nil
}

func classifyHTTP(forge string, verb vcs.Verb, resp *http.Response, body []byte) error {
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	msg := strings.TrimSpace(payload.Message)
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return vcs.Errorf(vcs.CodeForgeUnauthorized, forge, "%s; check the forge credential, do not retry", msg).WithVerb(verb)
	case http.StatusForbidden:
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			return vcs.Errorf(vcs.CodeForgeUnavailable, forge, "rate limited until %s: %s; wait before retrying", reset, msg).WithVerb(verb)
		}
		return vcs.Errorf(vcs.CodeForgeUnauthorized, forge, "%s; the credential lacks permission, do not retry", msg).WithVerb(verb)
	case http.StatusNotFound:
		return vcs.Errorf(vcs.CodeForgeRejected, forge, "not found: %s", msg).WithVerb(verb)
	case http.StatusUnprocessableEntity, http.StatusConflict, http.StatusBadRequest:
		return vcs.Errorf(vcs.CodeForgeRejected, forge, "%s; fix the request fields", msg).WithVerb(verb)
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return vcs.Errorf(vcs.CodeForgeUnavailable, forge, "%s; retry the call", msg).WithVerb(verb)
	default:
		return vcs.Errorf(vcs.CodeForgeCallFailed, forge, "HTTP %d: %s", resp.StatusCode, msg).WithVerb(verb)
	}
}

// Fixture is the httptest server half. It stores proposals in memory and
// can be armed to answer the next request with a forced status.
type Fixture struct {
	Server *httptest.Server
	CAFile string // PEM bundle trusting Server when started with NewTLSFixture

	mu        sync.Mutex
	nextErr   *forcedResponse
	proposals map[int]*vcs.Proposal
	comments  map[int][]vcs.Comment
	protected map[string]bool
	base      string
}

type forcedResponse struct {
	status int
	header map[string]string
	body   string
}

// NewFixture starts an HTTP fixture server.
func NewFixture(tb testing.TB) *Fixture {
	tb.Helper()
	fx := newFixture()
	fx.Server = httptest.NewServer(http.HandlerFunc(fx.serve))
	tb.Cleanup(fx.Server.Close)
	return fx
}

// NewTLSFixture starts a TLS fixture server with its own certificate and
// writes the certificate's PEM to CAFile.
func NewTLSFixture(tb testing.TB) *Fixture {
	tb.Helper()
	fx := newFixture()
	fx.Server = httptest.NewTLSServer(http.HandlerFunc(fx.serve))
	tb.Cleanup(fx.Server.Close)

	pemBytes := encodeCertPEM(fx.Server.Certificate().Raw)
	caPath := filepath.Join(tb.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		tb.Fatalf("writing CA bundle: %v", err)
	}
	fx.CAFile = caPath
	return fx
}

func newFixture() *Fixture {
	return &Fixture{
		proposals: map[int]*vcs.Proposal{},
		comments:  map[int][]vcs.Comment{},
		protected: map[string]bool{"main": true},
		base:      "main",
	}
}

// ForceNext makes the next request to the fixture return status with
// message in {"message": ...} and optional headers.
func (fx *Fixture) ForceNext(status int, message string, headers map[string]string) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.nextErr = &forcedResponse{status: status, header: headers, body: message}
}

// SetBaseBranch sets the default branch the fixture reports. Empty makes
// BaseBranch fail with BASE_BRANCH_MISSING.
func (fx *Fixture) SetBaseBranch(b string) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.base = b
}

func (fx *Fixture) serve(w http.ResponseWriter, r *http.Request) {
	fx.mu.Lock()
	defer fx.mu.Unlock()

	if fx.nextErr != nil {
		fe := fx.nextErr
		fx.nextErr = nil
		for k, v := range fe.header {
			w.Header().Set(k, v)
		}
		writeError(w, fe.status, fe.body)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+ValidToken {
		writeError(w, http.StatusUnauthorized, "bad token")
		return
	}

	// Routes sit under /api/repos/<2-or-3-seg-path>[/...]
	rest := strings.TrimPrefix(r.URL.Path, "/api/repos/")
	parts := strings.Split(rest, "/")
	tail, ok := splitRepoTail(parts)
	if !ok {
		writeError(w, http.StatusNotFound, "no such route")
		return
	}

	switch {
	case len(tail) == 0 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"default_branch": fx.base})

	case len(tail) == 2 && tail[0] == "branches" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]bool{"protected": fx.protected[tail[1]]})

	case len(tail) == 1 && tail[0] == "proposals" && r.Method == http.MethodPost:
		var req vcs.ProposalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		n := len(fx.proposals) + 1
		now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
		p := &vcs.Proposal{
			Number:    n,
			URL:       fmt.Sprintf("https://%s/proposals/%d", PublicHost, n),
			Title:     req.Title,
			Body:      req.Body,
			State:     vcs.ProposalOpen,
			Source:    req.Source,
			Target:    req.Target,
			Revision:  "0123456789abcdef0123456789abcdef01234567",
			Draft:     req.Draft,
			Author:    vcs.Actor{Login: "platform-agent", Bot: true},
			Labels:    append([]string(nil), req.Labels...),
			CreatedAt: now,
			UpdatedAt: now,
		}
		fx.proposals[n] = p
		writeJSON(w, http.StatusCreated, p)

	case len(tail) == 1 && tail[0] == "proposals" && r.Method == http.MethodGet:
		state := vcs.ProposalState(r.URL.Query().Get("state"))
		var list []vcs.Proposal
		for i := 1; i <= len(fx.proposals); i++ {
			p := fx.proposals[i]
			if state == "" || p.State == state {
				list = append(list, *p)
			}
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		truncated := false
		if limit > 0 && len(list) > limit {
			list = list[:limit]
			truncated = true
		}
		writeJSON(w, http.StatusOK, vcs.ProposalList{Proposals: list, Truncated: truncated})

	case len(tail) >= 2 && tail[0] == "proposals":
		n, err := strconv.Atoi(tail[1])
		p := fx.proposals[n]
		if err != nil || p == nil {
			writeError(w, http.StatusNotFound, "no such proposal")
			return
		}
		fx.serveProposal(w, r, n, p, tail[2:])

	default:
		writeError(w, http.StatusNotFound, "no such route")
	}
}

func (fx *Fixture) serveProposal(w http.ResponseWriter, r *http.Request, n int, p *vcs.Proposal, sub []string) {
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, p)

	case len(sub) == 0 && r.Method == http.MethodPatch:
		var u vcs.ProposalUpdate
		if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if u.Title != nil {
			p.Title = *u.Title
		}
		if u.Body != nil {
			p.Body = *u.Body
		}
		if u.Draft != nil {
			p.Draft = *u.Draft
		}
		p.Labels = append(p.Labels, u.AddLabels...)
		writeJSON(w, http.StatusOK, p)

	case len(sub) == 1 && sub[0] == "close" && r.Method == http.MethodPost:
		p.State = vcs.ProposalClosed
		writeJSON(w, http.StatusOK, p)

	case len(sub) == 1 && sub[0] == "comments" && r.Method == http.MethodPost:
		var in struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		c := vcs.Comment{
			ID:        strconv.Itoa(len(fx.comments[n]) + 1),
			Kind:      vcs.CommentDiscussion,
			Author:    vcs.Actor{Login: "platform-agent", Bot: true},
			Body:      in.Body,
			URL:       fmt.Sprintf("%s#comment-%d", p.URL, len(fx.comments[n])+1),
			CreatedAt: p.UpdatedAt,
		}
		fx.comments[n] = append(fx.comments[n], c)
		writeJSON(w, http.StatusCreated, c)

	case len(sub) == 1 && sub[0] == "comments" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, vcs.CommentList{Comments: fx.comments[n]})

	case len(sub) == 1 && sub[0] == "commits" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, vcs.CommitList{
			Commits: []vcs.Commit{{
				Revision: p.Revision,
				Message:  p.Title,
				Author:   vcs.Identity{Name: "platform-agent", Email: "bot@example"},
				When:     p.CreatedAt,
			}},
		})

	default:
		writeError(w, http.StatusNotFound, "no such proposal subroute")
	}
}

// splitRepoTail separates a 2- or 3-segment repository path from the
// route tail ("proposals", "branches", ...).
func splitRepoTail(parts []string) ([]string, bool) {
	for i, seg := range parts {
		if seg == "proposals" || seg == "branches" {
			if i < 2 || i > 3 {
				return nil, false
			}
			return parts[i:], true
		}
	}
	if len(parts) >= 2 && len(parts) <= 3 {
		return nil, true
	}
	return nil, false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func encodeCertPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
