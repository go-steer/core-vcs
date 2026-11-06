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

// Command gitops-workflow demonstrates an end-to-end pure-Go GitOps workflow
// with core-vcs: configuring a Catalog with CredentialFunc, cloning a
// repository via a shared bare object cache (Policy.CacheDir), creating a
// topic branch, editing a manifest, inspecting Diff and Log, publishing the
// branch, and opening a proposal.
//
// When run with no environment variables, it starts an in-process smart-HTTP
// bare Git server over TLS so the example is self-contained and requires no
// external network or credentials.
package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

	"github.com/go-steer/core-vcs/providers"
	"github.com/go-steer/core-vcs/vcs"
)

func main() {
	out, err := runWorkflow(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(out)
}

func runWorkflow(ctx context.Context) (string, error) {
	tmpRoot, err := os.MkdirTemp("", "core-vcs-example-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmpRoot) }()

	srv, host, port, caFile, cleanup, err := startLocalGitServer(tmpRoot, "acme/k8s-fleet")
	if err != nil {
		return "", err
	}
	defer cleanup()
	_ = srv

	// Build a Catalog using CredentialFunc (the adapter for an outbound
	// credential proxy or dynamic token minter).
	cat, err := vcs.NewCatalog(
		providers.NewRegistry(),
		[]vcs.Forge{
			{
				Name:     "internal-git",
				Provider: "git",
				Host:     host,
				Port:     port,
				CAFile:   caFile,
				Username: "git",
				Cred: vcs.CredentialFunc(func(context.Context) (string, error) {
					return "example-session-token", nil
				}),
				CommitIdentity: vcs.Identity{
					Name:  "gitops-agent",
					Email: "gitops-agent@acme.example",
				},
				ProposalURLTemplate: "https://{host}/{path}/+/compare/{base}...{branch}",
			},
		},
		[]vcs.RepoConfig{
			{
				Forge:      "internal-git",
				Repository: "acme/k8s-fleet",
				Role:       vcs.RoleGitOps,
				BaseBranch: "main",
			},
		},
	)
	if err != nil {
		return "", err
	}

	repo, ok := cat.GitOps()
	if !ok {
		return "", fmt.Errorf("no gitops repository configured")
	}

	// Clone using Policy.CacheDir so concurrent sessions share the bare
	// packfile store under <tmpRoot>/cache.
	pol := vcs.Policy{
		CacheDir: filepath.Join(tmpRoot, "cache"),
	}
	workDir := filepath.Join(tmpRoot, "session-1")
	ws, err := vcs.Clone(ctx, repo, "", workDir, pol)
	if err != nil {
		return "", err
	}

	if err := ws.Branch("agent/scale-replicas"); err != nil {
		return "", err
	}

	manifestPath := filepath.Join(ws.Dir(), "deploy/replicas.yaml")
	if err := os.WriteFile(manifestPath, []byte("replicas: 5\n"), 0o644); err != nil {
		return "", err
	}

	diffOut, err := ws.Diff(vcs.DiffOptions{AgainstBase: true})
	if err != nil {
		return "", err
	}

	cm, err := ws.Commit("feat(deploy): scale replicas from 2 to 5", vcs.Identity{})
	if err != nil {
		return "", err
	}

	pub, err := ws.Publish(ctx, "")
	if err != nil {
		return "", err
	}

	prop, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
		Source: pub.Branch,
		Title:  "feat(deploy): scale replicas from 2 to 5",
		Body:   "Automated replica bump prepared in an isolated worktree.",
	})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "branch:   %s\n", pub.Branch)
	fmt.Fprintf(&b, "commit:   %s\n", cm.Message)
	fmt.Fprintf(&b, "proposal: %s\n", prop.URL)
	fmt.Fprintf(&b, "diff:\n%s", diffOut)
	return b.String(), nil
}

type singleStorerLoader struct{ s storer.Storer }

func (l singleStorerLoader) Load(*transport.Endpoint) (storer.Storer, error) { return l.s, nil }

func startLocalGitServer(root, repoPath string) (*httptest.Server, string, int, string, func(), error) {
	bareDir := filepath.Join(root, "remote.git")
	store := filesystem.NewStorage(osfs.New(bareDir), cache.NewObjectLRUDefault())
	if _, err := git.Init(store, nil); err != nil {
		return nil, "", 0, "", nil, err
	}

	memStore := memory.NewStorage()
	wtFS := memfs.New()
	work, err := git.Init(memStore, wtFS)
	if err != nil {
		return nil, "", 0, "", nil, err
	}
	wt, err := work.Worktree()
	if err != nil {
		return nil, "", 0, "", nil, err
	}
	if err := wtFS.MkdirAll("deploy", 0o755); err != nil {
		return nil, "", 0, "", nil, err
	}
	f, err := wtFS.Create("deploy/replicas.yaml")
	if err != nil {
		return nil, "", 0, "", nil, err
	}
	_, _ = f.Write([]byte("replicas: 2\n"))
	_ = f.Close()
	if _, err := wt.Add("deploy/replicas.yaml"); err != nil {
		return nil, "", 0, "", nil, err
	}
	sig := &object.Signature{
		Name:  "seed",
		Email: "seed@acme.example",
		When:  time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
	}
	hash, err := wt.Commit("initial commit", &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		return nil, "", 0, "", nil, err
	}
	iter, err := memStore.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return nil, "", 0, "", nil, err
	}
	_ = iter.ForEach(func(obj plumbing.EncodedObject) error {
		_, err := store.SetEncodedObject(obj)
		return err
	})
	mainRef := plumbing.NewBranchReferenceName("main")
	if err := store.SetReference(plumbing.NewHashReference(mainRef, hash)); err != nil {
		return nil, "", 0, "", nil, err
	}
	if err := store.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, mainRef)); err != nil {
		return nil, "", 0, "", nil, err
	}

	prefix := "/" + repoPath + ".git"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix+"/") {
			http.NotFound(w, r)
			return
		}
		sub := strings.TrimPrefix(r.URL.Path, prefix)
		gitSrv := server.NewServer(singleStorerLoader{s: store})
		ep := &transport.Endpoint{Path: bareDir}

		switch {
		case sub == "/info/refs" && r.Method == http.MethodGet:
			svc := r.URL.Query().Get("service")
			var sess transport.Session
			var err error
			switch svc {
			case transport.UploadPackServiceName:
				sess, err = gitSrv.NewUploadPackSession(ep, nil)
			case transport.ReceivePackServiceName:
				sess, err = gitSrv.NewReceivePackSession(ep, nil)
			default:
				http.Error(w, "unsupported service", http.StatusForbidden)
				return
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer func() { _ = sess.Close() }()
			ar, err := sess.AdvertisedReferencesContext(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if ar.Capabilities != nil && len(ar.Capabilities.Get(capability.SymRef)) == 0 {
				_ = ar.Capabilities.Add(capability.SymRef, "HEAD:refs/heads/main")
			}
			ar.Prefix = [][]byte{[]byte("# service=" + svc), pktline.Flush}
			w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
			_ = ar.Encode(w)

		case sub == "/"+transport.UploadPackServiceName && r.Method == http.MethodPost:
			sess, err := gitSrv.NewUploadPackSession(ep, nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer func() { _ = sess.Close() }()
			req := packp.NewUploadPackRequest()
			if err := req.Decode(r.Body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			resp, err := sess.UploadPack(r.Context(), req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer func() { _ = resp.Close() }()
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			_ = resp.Encode(w)

		case sub == "/"+transport.ReceivePackServiceName && r.Method == http.MethodPost:
			sess, err := gitSrv.NewReceivePackSession(ep, nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer func() { _ = sess.Close() }()
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			req := packp.NewReferenceUpdateRequest()
			if err := req.Decode(bytes.NewReader(body)); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			status, _ := sess.ReceivePack(r.Context(), req)
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			if status != nil {
				_ = status.Encode(w)
			}

		default:
			http.NotFound(w, r)
		}
	}))

	u, err := url.Parse(srv.URL)
	if err != nil {
		srv.Close()
		return nil, "", 0, "", nil, err
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		srv.Close()
		return nil, "", 0, "", nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		srv.Close()
		return nil, "", 0, "", nil, err
	}
	caPath := filepath.Join(root, "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		srv.Close()
		return nil, "", 0, "", nil, err
	}
	return srv, host, port, caPath, srv.Close, nil
}
