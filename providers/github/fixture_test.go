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
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-vcs/providers/github"
)

const validFixtureToken = "ghp_fixture_secret_token_do_not_log"

type fixture struct {
	Server *httptest.Server
	CAFile string

	enterprise bool // true when routes sit under /api/v3

	mu             sync.Mutex
	nextForced     *forcedReply
	defaultBranch  string
	protected      map[string]bool
	knownBranches  map[string]bool
	pulls          map[int]*pullRecord
	issueComments  map[int][]map[string]any
	reviews        map[int][]map[string]any
	reviewComments map[int][]map[string]any
}

type forcedReply struct {
	status  int
	headers map[string]string
	body    any
}

type pullRecord struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	State     string   `json:"state"`
	Draft     bool     `json:"draft"`
	Merged    bool     `json:"merged"`
	HeadRef   string   `json:"-"`
	HeadSHA   string   `json:"-"`
	BaseRef   string   `json:"-"`
	Labels    []string `json:"-"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

func newTLSFixture(tb testing.TB, enterprise bool) *fixture {
	tb.Helper()
	fx := &fixture{
		enterprise:    enterprise,
		defaultBranch: "main",
		protected:     map[string]bool{"main": true},
		knownBranches: map[string]bool{"main": true, "agent/fix-1": true},
		pulls:         map[int]*pullRecord{},
		issueComments: map[int][]map[string]any{},
		reviews: map[int][]map[string]any{
			1: {
				{
					"id":           8001,
					"body":         "",
					"html_url":     "https://github.com/acme/infra/pull/1#pullrequestreview-8001",
					"user":         map[string]any{"login": "silent-approver", "type": "User"},
					"submitted_at": "2026-11-04T12:00:30Z",
				},
				{
					"id":           8002,
					"body":         "overall looks good after bounds check",
					"html_url":     "https://github.com/acme/infra/pull/1#pullrequestreview-8002",
					"user":         map[string]any{"login": "sre-lead", "type": "User"},
					"submitted_at": "2026-11-04T12:01:30Z",
				},
			},
		},
		reviewComments: map[int][]map[string]any{
			1: {{
				"id":            9001,
				"body":          "please check replica bounds",
				"path":          "deploy/deployment.yaml",
				"line":          0,
				"original_line": 42,
				"html_url":      "https://github.com/acme/infra/pull/1#discussion_r9001",
				"user":          map[string]any{"login": "reviewer-bot[bot]", "type": "Bot"},
				"created_at":    "2026-11-04T12:01:00Z",
			}},
		},
	}
	fx.Server = httptest.NewTLSServer(http.HandlerFunc(fx.serve))
	tb.Cleanup(fx.Server.Close)

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fx.Server.Certificate().Raw})
	caPath := filepath.Join(tb.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		tb.Fatalf("writing CA bundle: %v", err)
	}
	fx.CAFile = caPath
	return fx
}

func (fx *fixture) forceNext(status int, body any, headers map[string]string) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.nextForced = &forcedReply{status: status, headers: headers, body: body}
}

func (fx *fixture) serve(w http.ResponseWriter, r *http.Request) {
	fx.mu.Lock()
	defer fx.mu.Unlock()

	if fx.nextForced != nil {
		rep := fx.nextForced
		fx.nextForced = nil
		for k, v := range rep.headers {
			w.Header().Set(k, v)
		}
		writeFixtureJSON(w, rep.status, rep.body)
		return
	}

	if got := r.Header.Get("Accept"); got != github.AcceptHeader {
		writeFixtureJSON(w, http.StatusBadRequest, map[string]any{"message": "missing or wrong Accept header: " + got})
		return
	}
	if got := r.Header.Get("X-GitHub-Api-Version"); got != github.APIVersion {
		writeFixtureJSON(w, http.StatusBadRequest, map[string]any{"message": "missing or wrong X-GitHub-Api-Version: " + got})
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+validFixtureToken {
		writeFixtureJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
		return
	}

	path := r.URL.Path
	if fx.enterprise {
		if !strings.HasPrefix(path, github.EnterpriseAPIPrefix+"/") {
			writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "GHES route must start with /api/v3"})
			return
		}
		path = strings.TrimPrefix(path, github.EnterpriseAPIPrefix)
	} else if strings.HasPrefix(path, github.EnterpriseAPIPrefix+"/") {
		writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "public GitHub route must not start with /api/v3"})
		return
	}

	if !strings.HasPrefix(path, "/repos/") {
		writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/repos/"), "/")
	if len(parts) < 2 {
		writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	repoSlug := parts[0] + "/" + parts[1]
	tail := parts[2:]

	switch {
	case len(tail) == 0 && r.Method == http.MethodGet:
		writeFixtureJSON(w, http.StatusOK, map[string]any{
			"full_name":      repoSlug,
			"default_branch": fx.defaultBranch,
		})

	case len(tail) >= 2 && tail[0] == "branches" && r.Method == http.MethodGet:
		branch := strings.Join(tail[1:], "/")
		if !fx.knownBranches[branch] {
			writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "Branch not found"})
			return
		}
		writeFixtureJSON(w, http.StatusOK, map[string]any{
			"name":      branch,
			"protected": fx.protected[branch],
		})

	case len(tail) == 1 && tail[0] == "pulls" && r.Method == http.MethodPost:
		var in struct {
			Title string `json:"title"`
			Head  string `json:"head"`
			Base  string `json:"base"`
			Body  string `json:"body"`
			Draft bool   `json:"draft"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeFixtureJSON(w, http.StatusBadRequest, map[string]any{"message": err.Error()})
			return
		}
		num := len(fx.pulls) + 1
		rec := &pullRecord{
			Number:    num,
			Title:     in.Title,
			Body:      in.Body,
			State:     "open",
			Draft:     in.Draft,
			HeadRef:   in.Head,
			HeadSHA:   "0123456789abcdef0123456789abcdef01234567",
			BaseRef:   in.Base,
			CreatedAt: "2026-11-04T12:00:00Z",
			UpdatedAt: "2026-11-04T12:00:00Z",
		}
		fx.pulls[num] = rec
		writeFixtureJSON(w, http.StatusCreated, fx.renderPull(repoSlug, rec))

	case len(tail) == 1 && tail[0] == "pulls" && r.Method == http.MethodGet:
		state := r.URL.Query().Get("state")
		var out []map[string]any
		for i := 1; i <= len(fx.pulls); i++ {
			pr := fx.pulls[i]
			if state == "" || state == "all" || pr.State == state {
				out = append(out, fx.renderPull(repoSlug, pr))
			}
		}
		writeFixtureJSON(w, http.StatusOK, out)

	case len(tail) >= 2 && tail[0] == "pulls":
		num, err := strconv.Atoi(tail[1])
		pr := fx.pulls[num]
		if err != nil || pr == nil {
			writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		fx.servePullSubroute(w, r, repoSlug, pr, tail[2:])

	case len(tail) >= 3 && tail[0] == "issues":
		num, err := strconv.Atoi(tail[1])
		pr := fx.pulls[num]
		if err != nil || pr == nil {
			writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		fx.serveIssueSubroute(w, r, repoSlug, pr, tail[2:])

	default:
		writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
	}
}

func (fx *fixture) servePullSubroute(w http.ResponseWriter, r *http.Request, repoSlug string, pr *pullRecord, sub []string) {
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		writeFixtureJSON(w, http.StatusOK, fx.renderPull(repoSlug, pr))

	case len(sub) == 0 && r.Method == http.MethodPatch:
		var patch struct {
			Title *string `json:"title"`
			Body  *string `json:"body"`
			State *string `json:"state"`
			Draft *bool   `json:"draft"`
		}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeFixtureJSON(w, http.StatusBadRequest, map[string]any{"message": err.Error()})
			return
		}
		if patch.Title != nil {
			pr.Title = *patch.Title
		}
		if patch.Body != nil {
			pr.Body = *patch.Body
		}
		if patch.State != nil {
			pr.State = *patch.State
		}
		if patch.Draft != nil {
			pr.Draft = *patch.Draft
		}
		writeFixtureJSON(w, http.StatusOK, fx.renderPull(repoSlug, pr))

	case len(sub) == 1 && sub[0] == "commits" && r.Method == http.MethodGet:
		writeFixtureJSON(w, http.StatusOK, []map[string]any{{
			"sha": pr.HeadSHA,
			"commit": map[string]any{
				"message": pr.Title,
				"author": map[string]any{
					"name":  "Platform Agent",
					"email": "agent@example",
					"date":  time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
				},
			},
		}})

	case len(sub) == 1 && sub[0] == "reviews" && r.Method == http.MethodGet:
		writeFixtureJSON(w, http.StatusOK, fx.reviews[pr.Number])

	case len(sub) == 1 && sub[0] == "comments" && r.Method == http.MethodGet:
		writeFixtureJSON(w, http.StatusOK, fx.reviewComments[pr.Number])

	default:
		writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
	}
}

func (fx *fixture) serveIssueSubroute(w http.ResponseWriter, r *http.Request, repoSlug string, pr *pullRecord, sub []string) {
	switch {
	case len(sub) == 1 && sub[0] == "comments" && r.Method == http.MethodPost:
		var in struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		id := len(fx.issueComments[pr.Number]) + 1001
		cm := map[string]any{
			"id":         id,
			"body":       in.Body,
			"html_url":   fmt.Sprintf("https://%s/%s/pull/%d#issuecomment-%d", github.PublicHost, repoSlug, pr.Number, id),
			"user":       map[string]any{"login": "platform-agent[bot]", "type": "Bot"},
			"created_at": "2026-11-04T12:02:00Z",
		}
		fx.issueComments[pr.Number] = append(fx.issueComments[pr.Number], cm)
		writeFixtureJSON(w, http.StatusCreated, cm)

	case len(sub) == 1 && sub[0] == "comments" && r.Method == http.MethodGet:
		writeFixtureJSON(w, http.StatusOK, fx.issueComments[pr.Number])

	case len(sub) == 1 && sub[0] == "labels" && r.Method == http.MethodPost:
		var in struct {
			Labels []string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, l := range in.Labels {
			if !slices.Contains(pr.Labels, l) {
				pr.Labels = append(pr.Labels, l)
			}
		}
		writeFixtureJSON(w, http.StatusOK, renderLabels(pr.Labels))

	case len(sub) == 2 && sub[0] == "labels" && r.Method == http.MethodDelete:
		rm := sub[1]
		pr.Labels = slices.DeleteFunc(pr.Labels, func(l string) bool { return l == rm })
		writeFixtureJSON(w, http.StatusOK, renderLabels(pr.Labels))

	default:
		writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
	}
}

func (fx *fixture) renderPull(repoSlug string, pr *pullRecord) map[string]any {
	return map[string]any{
		"number":     pr.Number,
		"html_url":   fmt.Sprintf("https://%s/%s/pull/%d", github.PublicHost, repoSlug, pr.Number),
		"title":      pr.Title,
		"body":       pr.Body,
		"state":      pr.State,
		"draft":      pr.Draft,
		"merged":     pr.Merged,
		"user":       map[string]any{"login": "platform-agent[bot]", "type": "Bot"},
		"labels":     renderLabels(pr.Labels),
		"created_at": pr.CreatedAt,
		"updated_at": pr.UpdatedAt,
		"head": map[string]any{
			"ref": pr.HeadRef,
			"sha": pr.HeadSHA,
		},
		"base": map[string]any{
			"ref": pr.BaseRef,
		},
	}
}

func renderLabels(names []string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n})
	}
	return out
}

func writeFixtureJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
