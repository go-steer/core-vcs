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
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-vcs/providers/gitlab"
)

const validFixtureToken = "glpat-fixture-secret-token-do-not-log"

type fixture struct {
	Server *httptest.Server
	CAFile string

	mu                sync.Mutex
	nextForced        *forcedReply
	defaultBranch     string
	requireApproval   bool
	protectedBranches []string
	mrs               map[int]*mrRecord
	notes             map[int][]map[string]any
}

type forcedReply struct {
	pathSuffix string
	status     int
	headers    map[string]string
	body       any
}

type mrRecord struct {
	IID                     int      `json:"iid"`
	Title                   string   `json:"title"`
	Description             string   `json:"description"`
	State                   string   `json:"state"`
	Draft                   bool     `json:"draft"`
	Approved                bool     `json:"-"`
	CannotApprove           bool     `json:"-"`
	DetailedMergeStatus     string   `json:"-"`
	SourceBranch            string   `json:"source_branch"`
	TargetBranch            string   `json:"target_branch"`
	SHA                     string   `json:"sha"`
	AdvanceOnNextNotePost   string   `json:"-"`
	BaseSHA                 string   `json:"-"`
	StartSHA                string   `json:"-"`
	DiffHeadSHA             string   `json:"-"`
	HideDiffRefs            bool     `json:"-"`
	MergeCommitSHA          string   `json:"-"`
	SquashCommitSHA         string   `json:"-"`
	LastSquash              *bool    `json:"-"`
	LastMergeCommitMessage  string   `json:"-"`
	LastSquashCommitMessage string   `json:"-"`
	RemovedSourceBranch     bool     `json:"-"`
	Labels                  []string `json:"labels"`
	CreatedAt               string   `json:"created_at"`
	UpdatedAt               string   `json:"updated_at"`
}

func newTLSFixture(tb testing.TB) *fixture {
	tb.Helper()
	fx := &fixture{
		defaultBranch:     "main",
		protectedBranches: []string{"main", "release/*"},
		mrs:               map[int]*mrRecord{},
		notes: map[int][]map[string]any{
			1: {
				{
					"id":         501,
					"body":       "assigned to @sre",
					"system":     true,
					"author":     map[string]any{"username": "gitlab-system", "bot": true},
					"created_at": "2026-11-04T12:00:30Z",
				},
				{
					"id":         502,
					"body":       "please double-check limits.cpu",
					"system":     false,
					"author":     map[string]any{"username": "reviewer-bot[bot]", "bot": true},
					"created_at": "2026-11-04T12:01:00Z",
					"position": map[string]any{
						"new_path": "charts/values.yaml",
						"new_line": 27,
					},
				},
			},
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

func (fx *fixture) forceNextOnSuffix(pathSuffix string, status int, body any) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.nextForced = &forcedReply{pathSuffix: pathSuffix, status: status, body: body}
}

func (fx *fixture) hasPendingForced() bool {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return fx.nextForced != nil
}

func (fx *fixture) clearForced() {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.nextForced = nil
}

func (fx *fixture) setRequireApproval(req bool) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.requireApproval = req
}

func (fx *fixture) setMRDetailedMergeStatus(iid int, dms string) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if mr := fx.mrs[iid]; mr != nil {
		mr.DetailedMergeStatus = dms
	}
}

func (fx *fixture) setMRCannotApprove(iid int, blocked bool) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if mr := fx.mrs[iid]; mr != nil {
		mr.CannotApprove = blocked
	}
}

func (fx *fixture) setMRHideDiffRefs(iid int, hide bool) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if mr := fx.mrs[iid]; mr != nil {
		mr.HideDiffRefs = hide
	}
}

func (fx *fixture) setMRHeads(iid int, sha, diffHeadSHA string) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if mr := fx.mrs[iid]; mr != nil {
		mr.SHA = sha
		mr.DiffHeadSHA = diffHeadSHA
	}
}

func (fx *fixture) advanceMRHeadOnNextNotePost(iid int, newSHA string) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if mr := fx.mrs[iid]; mr != nil {
		mr.AdvanceOnNextNotePost = newSHA
	}
}

func (fx *fixture) snapshotMR(iid int) mrRecord {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if mr := fx.mrs[iid]; mr != nil {
		return *mr
	}
	return mrRecord{}
}

func (fx *fixture) serve(w http.ResponseWriter, r *http.Request) {
	fx.mu.Lock()
	defer fx.mu.Unlock()

	if fx.nextForced != nil && (fx.nextForced.pathSuffix == "" || strings.HasSuffix(r.URL.EscapedPath(), fx.nextForced.pathSuffix)) {
		rep := fx.nextForced
		fx.nextForced = nil
		for k, v := range rep.headers {
			w.Header().Set(k, v)
		}
		writeFixtureJSON(w, rep.status, rep.body)
		return
	}

	if got := r.Header.Get(gitlab.PrivateTokenHeader); got != validFixtureToken {
		writeFixtureJSON(w, http.StatusUnauthorized, map[string]any{"message": "401 Unauthorized"})
		return
	}

	escaped := r.URL.EscapedPath()
	const prefix = gitlab.APIPrefix + "/projects/"
	if !strings.HasPrefix(escaped, prefix) {
		writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not Found: route must start with /api/v4/projects/"})
		return
	}
	rest := strings.TrimPrefix(escaped, prefix)
	encodedProject, tailEscaped, _ := strings.Cut(rest, "/")
	if !strings.Contains(strings.ToLower(encodedProject), "%2f") {
		writeFixtureJSON(w, http.StatusBadRequest, map[string]any{
			"message": "project path must be URL-encoded as a single path segment, got " + encodedProject,
		})
		return
	}
	projectPath, err := url.PathUnescape(encodedProject)
	if err != nil || strings.Count(projectPath, "/") < 1 {
		writeFixtureJSON(w, http.StatusBadRequest, map[string]any{"message": "invalid encoded project path"})
		return
	}

	var tail []string
	if tailEscaped != "" {
		for _, seg := range strings.Split(tailEscaped, "/") {
			un, err := url.PathUnescape(seg)
			if err != nil {
				un = seg
			}
			tail = append(tail, un)
		}
	}

	switch {
	case len(tail) == 0 && r.Method == http.MethodGet:
		writeFixtureJSON(w, http.StatusOK, map[string]any{
			"path_with_namespace": projectPath,
			"default_branch":      fx.defaultBranch,
		})

	case len(tail) == 1 && tail[0] == "protected_branches" && r.Method == http.MethodGet:
		rules := make([]map[string]any, 0, len(fx.protectedBranches))
		for _, b := range fx.protectedBranches {
			rules = append(rules, map[string]any{"name": b})
		}
		writeFixtureJSON(w, http.StatusOK, rules)

	case len(tail) == 1 && tail[0] == "merge_requests" && r.Method == http.MethodPost:
		var in struct {
			SourceBranch string `json:"source_branch"`
			TargetBranch string `json:"target_branch"`
			Title        string `json:"title"`
			Description  string `json:"description"`
			Labels       string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeFixtureJSON(w, http.StatusBadRequest, map[string]any{"message": err.Error()})
			return
		}
		iid := len(fx.mrs) + 1
		var labels []string
		if in.Labels != "" {
			labels = strings.Split(in.Labels, ",")
		}
		draft := strings.HasPrefix(in.Title, "Draft:")
		rec := &mrRecord{
			IID:          iid,
			Title:        in.Title,
			Description:  in.Description,
			State:        "opened",
			Draft:        draft,
			SourceBranch: in.SourceBranch,
			TargetBranch: in.TargetBranch,
			SHA:          "fedcba9876543210fedcba9876543210fedcba98",
			BaseSHA:      "1111111111111111111111111111111111111111",
			StartSHA:     "1111111111111111111111111111111111111111",
			DiffHeadSHA:  "fedcba9876543210fedcba9876543210fedcba98",
			Labels:       labels,
			CreatedAt:    "2026-11-04T12:00:00Z",
			UpdatedAt:    "2026-11-04T12:00:00Z",
		}
		fx.mrs[iid] = rec
		writeFixtureJSON(w, http.StatusCreated, fx.renderMR(projectPath, rec))

	case len(tail) == 1 && tail[0] == "merge_requests" && r.Method == http.MethodGet:
		state := r.URL.Query().Get("state")
		var out []map[string]any
		for i := 1; i <= len(fx.mrs); i++ {
			mr := fx.mrs[i]
			if state == "" || state == "all" || mr.State == state {
				out = append(out, fx.renderMR(projectPath, mr))
			}
		}
		writeFixtureJSON(w, http.StatusOK, out)

	case len(tail) >= 2 && tail[0] == "merge_requests":
		iid, err := strconv.Atoi(tail[1])
		mr := fx.mrs[iid]
		if err != nil || mr == nil {
			writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not found"})
			return
		}
		fx.serveMRSubroute(w, r, projectPath, mr, tail[2:])

	default:
		writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not Found"})
	}
}

func (fx *fixture) serveMRSubroute(w http.ResponseWriter, r *http.Request, projectPath string, mr *mrRecord, sub []string) {
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		writeFixtureJSON(w, http.StatusOK, fx.renderMR(projectPath, mr))

	case len(sub) == 0 && r.Method == http.MethodPut:
		var in struct {
			Title        *string `json:"title"`
			Description  *string `json:"description"`
			StateEvent   *string `json:"state_event"`
			AddLabels    *string `json:"add_labels"`
			RemoveLabels *string `json:"remove_labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeFixtureJSON(w, http.StatusBadRequest, map[string]any{"message": err.Error()})
			return
		}
		if in.Title != nil {
			mr.Title = *in.Title
			mr.Draft = strings.HasPrefix(mr.Title, "Draft:")
		}
		if in.Description != nil {
			mr.Description = *in.Description
		}
		if in.StateEvent != nil && *in.StateEvent == "close" {
			mr.State = "closed"
		}
		if in.AddLabels != nil && *in.AddLabels != "" {
			for _, add := range strings.Split(*in.AddLabels, ",") {
				if !slices.Contains(mr.Labels, add) {
					mr.Labels = append(mr.Labels, add)
				}
			}
		}
		if in.RemoveLabels != nil && *in.RemoveLabels != "" {
			for _, rm := range strings.Split(*in.RemoveLabels, ",") {
				mr.Labels = slices.DeleteFunc(mr.Labels, func(l string) bool { return l == rm })
			}
		}
		writeFixtureJSON(w, http.StatusOK, fx.renderMR(projectPath, mr))

	case len(sub) == 1 && sub[0] == "notes" && r.Method == http.MethodPost:
		var in struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		id := len(fx.notes[mr.IID]) + 601
		note := map[string]any{
			"id":         id,
			"body":       in.Body,
			"system":     false,
			"author":     map[string]any{"username": "platform-agent", "bot": true},
			"created_at": "2026-11-04T12:02:00Z",
		}
		fx.notes[mr.IID] = append(fx.notes[mr.IID], note)
		if mr.AdvanceOnNextNotePost != "" {
			mr.SHA = mr.AdvanceOnNextNotePost
			mr.DiffHeadSHA = mr.AdvanceOnNextNotePost
			mr.AdvanceOnNextNotePost = ""
		}
		writeFixtureJSON(w, http.StatusCreated, note)

	case len(sub) == 1 && sub[0] == "notes" && r.Method == http.MethodGet:
		writeFixtureJSON(w, http.StatusOK, fx.notes[mr.IID])

	case len(sub) == 1 && sub[0] == "discussions" && r.Method == http.MethodPost:
		var in struct {
			Body     string `json:"body"`
			Position struct {
				PositionType string `json:"position_type"`
				BaseSHA      string `json:"base_sha"`
				StartSHA     string `json:"start_sha"`
				HeadSHA      string `json:"head_sha"`
				NewPath      string `json:"new_path"`
				NewLine      int    `json:"new_line"`
			} `json:"position"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeFixtureJSON(w, http.StatusBadRequest, map[string]any{"message": err.Error()})
			return
		}
		if in.Position.PositionType != "text" ||
			in.Position.BaseSHA == "" ||
			in.Position.StartSHA == "" ||
			in.Position.HeadSHA == "" ||
			in.Position.NewPath == "" ||
			in.Position.NewLine <= 0 ||
			strings.TrimSpace(in.Body) == "" {
			writeFixtureJSON(w, http.StatusBadRequest, map[string]any{"message": "400 Bad request - invalid diff position"})
			return
		}
		id := len(fx.notes[mr.IID]) + 701
		note := map[string]any{
			"id":         id,
			"body":       in.Body,
			"system":     false,
			"author":     map[string]any{"username": "reviewer-agent[bot]", "bot": true},
			"created_at": "2026-11-04T12:02:30Z",
			"position": map[string]any{
				"position_type": in.Position.PositionType,
				"base_sha":      in.Position.BaseSHA,
				"start_sha":     in.Position.StartSHA,
				"head_sha":      in.Position.HeadSHA,
				"new_path":      in.Position.NewPath,
				"new_line":      in.Position.NewLine,
			},
		}
		fx.notes[mr.IID] = append(fx.notes[mr.IID], note)
		if mr.AdvanceOnNextNotePost != "" {
			mr.SHA = mr.AdvanceOnNextNotePost
			mr.DiffHeadSHA = mr.AdvanceOnNextNotePost
			mr.AdvanceOnNextNotePost = ""
		}
		writeFixtureJSON(w, http.StatusCreated, map[string]any{
			"id":    fmt.Sprintf("disc-%d", id),
			"notes": []map[string]any{note},
		})

	case len(sub) == 1 && sub[0] == "approve" && r.Method == http.MethodPost:
		var in struct {
			SHA string `json:"sha"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&in)
		}
		// Real GitLab POST /approve returns 401 when the caller has already
		// approved or is barred from approving (e.g. author self-approval).
		if mr.Approved || mr.CannotApprove {
			writeFixtureJSON(w, http.StatusUnauthorized, map[string]any{"message": "401 Unauthorized"})
			return
		}
		if in.SHA != "" && in.SHA != mr.SHA {
			writeFixtureJSON(w, http.StatusConflict, map[string]any{"message": "SHA does not match HEAD of source branch"})
			return
		}
		mr.Approved = true
		writeFixtureJSON(w, http.StatusCreated, fx.renderApprovals(mr))

	case len(sub) == 1 && sub[0] == "approvals" && r.Method == http.MethodGet:
		writeFixtureJSON(w, http.StatusOK, fx.renderApprovals(mr))

	case len(sub) == 1 && sub[0] == "unapprove" && r.Method == http.MethodPost:
		if !mr.Approved {
			writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not found"})
			return
		}
		mr.Approved = false
		writeFixtureJSON(w, http.StatusCreated, fx.renderApprovals(mr))

	case len(sub) == 1 && sub[0] == "merge" && r.Method == http.MethodPut:
		var in struct {
			SHA                      string `json:"sha"`
			Squash                   *bool  `json:"squash"`
			MergeCommitMessage       string `json:"merge_commit_message"`
			SquashCommitMessage      string `json:"squash_commit_message"`
			ShouldRemoveSourceBranch bool   `json:"should_remove_source_branch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeFixtureJSON(w, http.StatusBadRequest, map[string]any{"message": err.Error()})
			return
		}
		if mr.State != "opened" {
			writeFixtureJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "405 Method Not Allowed"})
			return
		}
		if in.SHA != "" && in.SHA != mr.SHA {
			writeFixtureJSON(w, http.StatusConflict, map[string]any{"message": "SHA does not match HEAD of source branch: " + mr.SHA})
			return
		}
		switch dms := fx.effectiveDetailedMergeStatus(mr); dms {
		case "mergeable":
			// Proceed to merge.
		case "conflict":
			writeFixtureJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Branch cannot be merged"})
			return
		default:
			// Real GitLab returns a generic 405 body for draft, not_approved,
			// ci_must_pass, discussions_not_resolved, etc.
			writeFixtureJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "405 Method Not Allowed"})
			return
		}
		mr.State = "merged"
		mr.LastSquash = in.Squash
		mr.LastMergeCommitMessage = in.MergeCommitMessage
		mr.LastSquashCommitMessage = in.SquashCommitMessage
		mr.RemovedSourceBranch = in.ShouldRemoveSourceBranch
		if in.Squash != nil && *in.Squash {
			mr.SquashCommitSHA = "aaaa111122223333444455556666777788889999"
		} else {
			mr.MergeCommitSHA = "bbbb111122223333444455556666777788889999"
		}
		writeFixtureJSON(w, http.StatusOK, fx.renderMR(projectPath, mr))

	case len(sub) == 1 && sub[0] == "commits" && r.Method == http.MethodGet:
		writeFixtureJSON(w, http.StatusOK, []map[string]any{{
			"id":            mr.SHA,
			"title":         mr.Title,
			"message":       mr.Title,
			"author_name":   "Platform Agent",
			"author_email":  "agent@example",
			"authored_date": time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
		}})

	default:
		writeFixtureJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not found"})
	}
}

func (fx *fixture) effectiveDetailedMergeStatus(mr *mrRecord) string {
	if mr.DetailedMergeStatus != "" {
		return mr.DetailedMergeStatus
	}
	if mr.State != "opened" {
		return "not_open"
	}
	if mr.Draft {
		return "draft_status"
	}
	if fx.requireApproval && !mr.Approved {
		return "not_approved"
	}
	return "mergeable"
}

func (fx *fixture) renderApprovals(mr *mrRecord) map[string]any {
	approvedBy := []map[string]any{}
	if mr.Approved {
		approvedBy = append(approvedBy, map[string]any{
			"user": map[string]any{"username": "reviewer-agent[bot]", "bot": true},
		})
	}
	return map[string]any{
		"id":                mr.IID,
		"iid":               mr.IID,
		"sha":               mr.SHA,
		"approved":          mr.Approved,
		"user_has_approved": mr.Approved,
		"user_can_approve":  !mr.Approved && !mr.CannotApprove,
		"updated_at":        "2026-11-04T12:03:00Z",
		"approved_by":       approvedBy,
	}
}

func (fx *fixture) renderMR(projectPath string, mr *mrRecord) map[string]any {
	out := map[string]any{
		"iid":                   mr.IID,
		"web_url":               fmt.Sprintf("https://%s/%s/-/merge_requests/%d", gitlab.PublicHost, projectPath, mr.IID),
		"title":                 mr.Title,
		"description":           mr.Description,
		"state":                 mr.State,
		"draft":                 mr.Draft,
		"detailed_merge_status": fx.effectiveDetailedMergeStatus(mr),
		"source_branch":         mr.SourceBranch,
		"target_branch":         mr.TargetBranch,
		"sha":                   mr.SHA,
		"merge_commit_sha":      mr.MergeCommitSHA,
		"squash_commit_sha":     mr.SquashCommitSHA,
		"author":                map[string]any{"username": "platform-agent", "bot": true},
		"labels":                mr.Labels,
		"created_at":            mr.CreatedAt,
		"updated_at":            mr.UpdatedAt,
	}
	if !mr.HideDiffRefs {
		diffHead := mr.DiffHeadSHA
		if diffHead == "" {
			diffHead = mr.SHA
		}
		baseSHA := mr.BaseSHA
		if baseSHA == "" {
			baseSHA = "1111111111111111111111111111111111111111"
		}
		startSHA := mr.StartSHA
		if startSHA == "" {
			startSHA = baseSHA
		}
		out["diff_refs"] = map[string]any{
			"base_sha":  baseSHA,
			"start_sha": startSHA,
			"head_sha":  diffHead,
		}
	}
	return out
}

func writeFixtureJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
