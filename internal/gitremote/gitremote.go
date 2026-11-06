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

// Package gitremote serves a real bare go-git repository over httptest
// using the smart-HTTP git protocol (git-upload-pack and git-receive-pack),
// in pure Go with no git binary. Both Workspace tests (#3) and the bare-git
// provider contract test (#4) run against it.
package gitremote

import (
	"bytes"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Remote is a bare git repository served over HTTP or HTTPS.
type Remote struct {
	Server *httptest.Server
	// RepoPath is the URL path of the repository on Server, e.g. "acme/infra".
	RepoPath string
	// CAFile is the PEM certificate bundle when started with NewTLS.
	CAFile string

	dir   string
	bare  *git.Repository
	store *filesystem.Storage

	mu         sync.Mutex
	user       string
	pass       string
	rejectPush string
	protected  map[string]bool
}

// New creates a bare repository on disk with an initial commit on "main"
// and serves it over HTTP.
func New(tb testing.TB, repoPath string, files map[string]string) *Remote {
	tb.Helper()
	r := newRemote(tb, repoPath, files)
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	tb.Cleanup(r.Server.Close)
	return r
}

// NewTLS is New over HTTPS with a self-signed certificate written to CAFile.
func NewTLS(tb testing.TB, repoPath string, files map[string]string) *Remote {
	tb.Helper()
	r := newRemote(tb, repoPath, files)
	r.Server = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	tb.Cleanup(r.Server.Close)

	caPath := filepath.Join(tb.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.Server.Certificate().Raw})
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		tb.Fatalf("writing CA bundle: %v", err)
	}
	r.CAFile = caPath
	return r
}

func newRemote(tb testing.TB, repoPath string, files map[string]string) *Remote {
	tb.Helper()
	if repoPath == "" {
		repoPath = "acme/infra"
	}
	dir := filepath.Join(tb.TempDir(), "bare.git")
	fs := osfs.New(dir)
	store := filesystem.NewStorage(fs, cache.NewObjectLRUDefault())
	bare, err := git.Init(store, nil)
	if err != nil {
		tb.Fatalf("git.Init bare: %v", err)
	}
	r := &Remote{
		RepoPath:  strings.Trim(repoPath, "/"),
		dir:       dir,
		bare:      bare,
		store:     store,
		protected: map[string]bool{"main": true},
	}
	if files == nil {
		files = map[string]string{"README.md": "# acme/infra\n"}
	}
	r.CommitOnBranch(tb, "main", "initial commit", files, false)
	if err := store.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		tb.Fatalf("set HEAD: %v", err)
	}
	return r
}

// URL returns the smart-HTTP clone URL for the repository.
func (r *Remote) URL() string {
	return r.Server.URL + "/" + r.RepoPath + ".git"
}

// RequireAuth makes the server demand user/pass via HTTP Basic Auth on
// every git request.
func (r *Remote) RequireAuth(user, pass string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.user, r.pass = user, pass
}

// RejectNextPush makes the next git-receive-pack fail with reason.
func (r *Remote) RejectNextPush(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejectPush = reason
}

// SetProtected marks branch protected on the remote's fake forge route.
func (r *Remote) SetProtected(branch string, protected bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.protected[branch] = protected
}

// DeleteBranch removes branch from the bare remote.
func (r *Remote) DeleteBranch(tb testing.TB, branch string) {
	tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.store.RemoveReference(plumbing.NewBranchReferenceName(branch)); err != nil {
		tb.Fatalf("RemoveReference(%s): %v", branch, err)
	}
}

// SetHEAD points the bare remote's HEAD symbolic reference at branch.
// Passing an unborn branch name makes HEAD dangle so BaseBranch reports
// BASE_BRANCH_MISSING.
func (r *Remote) SetHEAD(tb testing.TB, branch string) {
	tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	target := plumbing.NewBranchReferenceName(branch)
	if err := r.store.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, target)); err != nil {
		tb.Fatalf("SetHEAD(%s): %v", branch, err)
	}
}

// BranchHash returns the current commit hash of branch on the bare remote,
// or "" when the branch does not exist.
func (r *Remote) BranchHash(branch string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, err := r.bare.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return ""
	}
	return ref.Hash().String()
}

// CommitOnBranch creates a commit on branch directly in the bare remote.
// When orphan is false and branch already exists, the new commit builds on
// its tip (advancing the branch); when orphan is true, the commit has no
// parent (rewriting the branch to an unrelated history, for BASE_MOVED).
func (r *Remote) CommitOnBranch(tb testing.TB, branch, msg string, files map[string]string, orphan bool) string {
	tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	memStore := memory.NewStorage()
	if existingIter, err := r.store.IterEncodedObjects(plumbing.AnyObject); err == nil {
		_ = existingIter.ForEach(func(obj plumbing.EncodedObject) error {
			_, err := memStore.SetEncodedObject(obj)
			return err
		})
	}
	wtFS := memfs.New()
	work, err := git.Init(memStore, wtFS)
	if err != nil {
		tb.Fatalf("git.Init mem: %v", err)
	}
	wt, err := work.Worktree()
	if err != nil {
		tb.Fatalf("Worktree: %v", err)
	}

	var parents []plumbing.Hash
	refName := plumbing.NewBranchReferenceName(branch)
	if !orphan {
		if existing, err := r.bare.Reference(refName, true); err == nil {
			parents = []plumbing.Hash{existing.Hash()}
			parentCommit, err := r.bare.CommitObject(existing.Hash())
			if err != nil {
				tb.Fatalf("CommitObject: %v", err)
			}
			tree, err := parentCommit.Tree()
			if err != nil {
				tb.Fatalf("Tree: %v", err)
			}
			_ = tree.Files().ForEach(func(f *object.File) error {
				data, err := f.Contents()
				if err != nil {
					return err
				}
				if err := writeMemFile(wtFS, f.Name, []byte(data)); err != nil {
					return err
				}
				_, err = wt.Add(f.Name)
				return err
			})
		}
	}

	for path, content := range files {
		if err := writeMemFile(wtFS, path, []byte(content)); err != nil {
			tb.Fatalf("writeMemFile(%s): %v", path, err)
		}
		if _, err := wt.Add(path); err != nil {
			tb.Fatalf("wt.Add(%s): %v", path, err)
		}
	}
	sig := &object.Signature{
		Name:  "remote-seed",
		Email: "seed@example",
		When:  time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC),
	}
	hash, err := wt.Commit(msg, &git.CommitOptions{
		Author:            sig,
		Committer:         sig,
		Parents:           parents,
		AllowEmptyCommits: true,
	})
	if err != nil {
		tb.Fatalf("wt.Commit: %v", err)
	}

	iter, err := memStore.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		tb.Fatalf("IterEncodedObjects: %v", err)
	}
	_ = iter.ForEach(func(obj plumbing.EncodedObject) error {
		_, err := r.store.SetEncodedObject(obj)
		return err
	})
	if err := r.store.SetReference(plumbing.NewHashReference(refName, hash)); err != nil {
		tb.Fatalf("SetReference: %v", err)
	}
	return hash.String()
}

func writeMemFile(fs billy.Filesystem, path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "." && dir != "/" {
		if err := fs.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := fs.Create(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

type singleLoader struct{ s storer.Storer }

func (l singleLoader) Load(*transport.Endpoint) (storer.Storer, error) { return l.s, nil }

func (r *Remote) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.user != "" || r.pass != "" {
		u, p, ok := req.BasicAuth()
		if !ok || u != r.user || p != r.pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
	}

	prefix := "/" + r.RepoPath + ".git"
	if !strings.HasPrefix(req.URL.Path, prefix+"/") {
		http.NotFound(w, req)
		return
	}
	sub := strings.TrimPrefix(req.URL.Path, prefix)
	srv := server.NewServer(singleLoader{s: r.store})
	ep := &transport.Endpoint{Path: r.dir}

	switch {
	case sub == "/info/refs" && req.Method == http.MethodGet:
		svc := req.URL.Query().Get("service")
		var sess transport.Session
		var err error
		switch svc {
		case transport.UploadPackServiceName:
			sess, err = srv.NewUploadPackSession(ep, nil)
		case transport.ReceivePackServiceName:
			sess, err = srv.NewReceivePackSession(ep, nil)
		default:
			http.Error(w, "unsupported service", http.StatusForbidden)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = sess.Close() }()
		ar, err := sess.AdvertisedReferencesContext(req.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if headRef, err := r.store.Reference(plumbing.HEAD); err == nil && headRef.Type() == plumbing.SymbolicReference {
			if ar.Capabilities != nil && len(ar.Capabilities.Get(capability.SymRef)) == 0 {
				_ = ar.Capabilities.Add(capability.SymRef, "HEAD:"+headRef.Target().String())
			}
		}
		ar.Prefix = [][]byte{[]byte("# service=" + svc), pktline.Flush}
		w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
		_ = ar.Encode(w)

	case sub == "/"+transport.UploadPackServiceName && req.Method == http.MethodPost:
		sess, err := srv.NewUploadPackSession(ep, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = sess.Close() }()
		upReq := packp.NewUploadPackRequest()
		if err := upReq.Decode(req.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := sess.UploadPack(req.Context(), upReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = resp.Close() }()
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		_ = resp.Encode(w)

	case sub == "/"+transport.ReceivePackServiceName && req.Method == http.MethodPost:
		sess, err := srv.NewReceivePackSession(ep, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = sess.Close() }()
		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		upReq := packp.NewReferenceUpdateRequest()
		if err := upReq.Decode(bytes.NewReader(bodyBytes)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.rejectPush != "" {
			reason := r.rejectPush
			r.rejectPush = ""
			status := packp.NewReportStatus()
			status.UnpackStatus = "ok"
			for _, cmd := range upReq.Commands {
				status.CommandStatuses = append(status.CommandStatuses, &packp.CommandStatus{
					ReferenceName: cmd.Name,
					Status:        reason,
				})
			}
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			_ = status.Encode(w)
			return
		}
		status, _ := sess.ReceivePack(req.Context(), upReq)
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		if status != nil {
			_ = status.Encode(w)
		}

	default:
		http.NotFound(w, req)
	}
}
