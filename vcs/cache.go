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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// cloneViaCache materialises a working tree at dir whose historical git
// objects live in a shared bare repository under pol.CacheDir.
//
// Concurrency and multi-process safety:
//  1. A per-repository lock file (<cache>/<host>/<path>.lock) is locked via
//     go-billy's OS file lock (flock on POSIX) only while updating the bare
//     object store with an incremental Fetch of branch.
//  2. The session worktree at dir has its own private .git/ directory (HEAD,
//     index, refs, config, and objects/ for commits created by this session)
//     and reads historical objects from the shared bare store via a layered
//     storage.Storer (overlayStorage).
//  3. Once Clone returns, the lock is released. Sessions never write to the
//     shared bare store during Branch, Commit, Status, Diff, Log or Publish.
func cloneViaCache(
	ctx context.Context,
	repo Repo,
	branch, dir string,
	pol Policy,
	gitAuth transport.AuthMethod,
	caBundle []byte,
) (*git.Repository, error) {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return nil, Errorf(CodeGitFailed, repo.Forge.Name, "clone path %s is not a directory", dir).WithVerb(VerbClone)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, Errorf(CodeGitFailed, repo.Forge.Name, "reading clone directory %s: %w", dir, err).WithVerb(VerbClone)
		}
		if len(entries) > 0 {
			return nil, Errorf(CodeGitFailed, repo.Forge.Name, "clone directory %s already exists and is not empty", dir).WithVerb(VerbClone)
		}
	} else if !os.IsNotExist(err) {
		return nil, Errorf(CodeGitFailed, repo.Forge.Name, "stat clone directory %s: %w", dir, err).WithVerb(VerbClone)
	}

	bareDir, err := repoCacheDir(pol.CacheDir, repo)
	if err != nil {
		return nil, err
	}

	commitHash, err := syncBareCache(ctx, repo, branch, bareDir, gitAuth, caBundle)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, Errorf(CodeGitFailed, repo.Forge.Name, "creating worktree directory %s: %w", dir, err).WithVerb(VerbClone)
	}

	wtFS := osfs.New(dir)
	dotFS, err := wtFS.Chroot(git.GitDirName)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, Errorf(CodeGitFailed, repo.Forge.Name, "creating .git chroot: %w", err).WithVerb(VerbClone)
	}

	sharedCache := cache.NewObjectLRUDefault()
	localStore := filesystem.NewStorage(dotFS, sharedCache)
	if err := localStore.Init(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, Errorf(CodeGitFailed, repo.Forge.Name, "initializing worktree storage: %w", err).WithVerb(VerbClone)
	}

	// Write .git/objects/info/alternates pointing at the shared bare objects
	// directory so standard git tooling inspecting the worktree sees the
	// same object graph.
	altObjectsDir := filepath.Join(bareDir, "objects") + "\n"
	altFile := filepath.Join(dir, git.GitDirName, "objects", "info", "alternates")
	if err := os.WriteFile(altFile, []byte(altObjectsDir), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return nil, Errorf(CodeGitFailed, repo.Forge.Name, "writing alternates file: %w", err).WithVerb(VerbClone)
	}

	bareStore := filesystem.NewStorageWithOptions(
		osfs.New(bareDir),
		sharedCache,
		filesystem.Options{ExclusiveAccess: false},
	)
	combinedStore := &overlayStorage{
		Storage: localStore,
		shared:  &bareStore.ObjectStorage,
	}

	cfg := config.NewConfig()
	cfg.Core.IsBare = false
	branchRef := plumbing.NewBranchReferenceName(branch)
	cfg.Branches[branch] = &config.Branch{
		Name:   branch,
		Remote: git.DefaultRemoteName,
		Merge:  branchRef,
	}
	if err := combinedStore.SetConfig(cfg); err != nil {
		_ = os.RemoveAll(dir)
		return nil, Errorf(CodeGitFailed, repo.Forge.Name, "writing worktree config: %w", err).WithVerb(VerbClone)
	}

	if err := combinedStore.SetReference(plumbing.NewHashReference(branchRef, commitHash)); err != nil {
		_ = os.RemoveAll(dir)
		return nil, Errorf(CodeGitFailed, repo.Forge.Name, "setting branch reference: %w", err).WithVerb(VerbClone)
	}
	if err := combinedStore.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branchRef)); err != nil {
		_ = os.RemoveAll(dir)
		return nil, Errorf(CodeGitFailed, repo.Forge.Name, "setting HEAD reference: %w", err).WithVerb(VerbClone)
	}

	gitRepo, err := git.Open(combinedStore, wtFS)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, Errorf(CodeGitFailed, repo.Forge.Name, "opening worktree repository: %w", err).WithVerb(VerbClone)
	}
	wt, err := gitRepo.Worktree()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, Errorf(CodeGitFailed, repo.Forge.Name, "opening worktree: %w", err).WithVerb(VerbClone)
	}
	if err := wt.Reset(&git.ResetOptions{
		Commit: commitHash,
		Mode:   git.HardReset,
	}); err != nil {
		_ = os.RemoveAll(dir)
		return nil, Errorf(CodeGitFailed, repo.Forge.Name, "checking out %s (%s): %w", branch, commitHash, err).WithVerb(VerbClone)
	}

	return gitRepo, nil
}

func repoCacheDir(cacheRoot string, repo Repo) (string, error) {
	absRoot, err := filepath.Abs(cacheRoot)
	if err != nil {
		return "", Errorf(CodeConfigInvalid, repo.Forge.Name, "resolving CacheDir %q: %w", cacheRoot, err).WithVerb(VerbClone)
	}
	if strings.ContainsAny(repo.Ref.Host, `/\`) || strings.Contains(repo.Ref.Host, "..") {
		return "", Errorf(CodeRepoInvalid, repo.Forge.Name, "repository %s has invalid host %q", repo.Ref, repo.Ref.Host).WithVerb(VerbClone)
	}
	hostDir := strings.ReplaceAll(repo.Ref.Host, ":", "_")
	if hostDir == "" {
		hostDir = "_default"
	}
	elems := append([]string{absRoot, hostDir}, repo.Ref.Segments()...)
	bareDir := filepath.Join(elems...) + ".git"
	rel, err := filepath.Rel(absRoot, bareDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", Errorf(CodeRepoInvalid, repo.Forge.Name, "repository %s escapes CacheDir", repo.Ref).WithVerb(VerbClone)
	}
	return bareDir, nil
}

func syncBareCache(
	ctx context.Context,
	repo Repo,
	branch, bareDir string,
	gitAuth transport.AuthMethod,
	caBundle []byte,
) (plumbing.Hash, error) {
	parentDir := filepath.Dir(bareDir)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return plumbing.ZeroHash, Errorf(CodeGitFailed, repo.Forge.Name, "creating cache directory %s: %w", parentDir, err).WithVerb(VerbClone)
	}

	// Acquire an inter-process lock on <parent>/<repo>.git.lock using
	// go-billy's OS file lock (flock on POSIX), retrying on EINTR if a Go
	// runtime preemption signal interrupts the blocking flock syscall.
	lockFS := osfs.New(parentDir)
	lockFile, err := lockFS.OpenFile(filepath.Base(bareDir)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return plumbing.ZeroHash, Errorf(CodeGitFailed, repo.Forge.Name, "opening cache lock in %s: %w", parentDir, err).WithVerb(VerbClone)
	}
	defer func() { _ = lockFile.Close() }()
	for {
		err = lockFile.Lock()
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return plumbing.ZeroHash, Errorf(CodeGitFailed, repo.Forge.Name, "locking cache in %s: %w", parentDir, err).WithVerb(VerbClone)
	}
	defer func() { _ = lockFile.Unlock() }()

	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		return plumbing.ZeroHash, Errorf(CodeGitFailed, repo.Forge.Name, "creating bare cache %s: %w", bareDir, err).WithVerb(VerbClone)
	}

	bareStore := filesystem.NewStorageWithOptions(
		osfs.New(bareDir),
		cache.NewObjectLRUDefault(),
		filesystem.Options{ExclusiveAccess: false},
	)
	if _, err := os.Stat(filepath.Join(bareDir, "config")); os.IsNotExist(err) {
		if _, err := git.Init(bareStore, nil); err != nil && !errors.Is(err, git.ErrRepositoryAlreadyExists) {
			return plumbing.ZeroHash, Errorf(CodeGitFailed, repo.Forge.Name, "initializing bare cache %s: %w", bareDir, err).WithVerb(VerbClone)
		}
	}

	rem := git.NewRemote(bareStore, &config.RemoteConfig{
		Name: git.DefaultRemoteName,
		URLs: []string{repo.CloneURL()},
	})
	branchRef := plumbing.NewBranchReferenceName(branch)
	spec := config.RefSpec(fmt.Sprintf("+%s:%s", branchRef, branchRef))
	err = rem.FetchContext(ctx, &git.FetchOptions{
		RemoteName: git.DefaultRemoteName,
		RefSpecs:   []config.RefSpec{spec},
		Auth:       gitAuth,
		CABundle:   caBundle,
		Tags:       git.NoTags,
		Force:      true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return plumbing.ZeroHash, classifyCloneError(repo, branch, err)
	}

	ref, err := bareStore.Reference(branchRef)
	if err != nil {
		return plumbing.ZeroHash, classifyCloneError(repo, branch, err)
	}
	return ref.Hash(), nil
}

// overlayStorage wraps a worktree's private *filesystem.Storage and falls back
// to a shared read-only *filesystem.ObjectStorage for object reads. All writes
// (new commits, trees, blobs, index, refs, config) go exclusively to the
// worktree's private Storage.
type overlayStorage struct {
	*filesystem.Storage
	shared *filesystem.ObjectStorage
}

func (o *overlayStorage) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	obj, err := o.shared.EncodedObject(t, h)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return o.Storage.EncodedObject(t, h)
	}
	return obj, err
}

func (o *overlayStorage) DeltaObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	obj, err := o.shared.DeltaObject(t, h)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return o.Storage.DeltaObject(t, h)
	}
	return obj, err
}

func (o *overlayStorage) HasEncodedObject(h plumbing.Hash) error {
	err := o.shared.HasEncodedObject(h)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return o.Storage.HasEncodedObject(h)
	}
	return err
}

func (o *overlayStorage) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	sz, err := o.shared.EncodedObjectSize(h)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return o.Storage.EncodedObjectSize(h)
	}
	return sz, err
}

func (o *overlayStorage) HashesWithPrefix(prefix []byte) ([]plumbing.Hash, error) {
	local, err := o.Storage.HashesWithPrefix(prefix)
	if err != nil {
		return nil, err
	}
	shared, err := o.shared.HashesWithPrefix(prefix)
	if err != nil {
		return nil, err
	}
	seen := make(map[plumbing.Hash]struct{}, len(local))
	for _, h := range local {
		seen[h] = struct{}{}
	}
	for _, h := range shared {
		if _, ok := seen[h]; !ok && bytes.HasPrefix(h[:], prefix) {
			local = append(local, h)
		}
	}
	return local, nil
}

func (o *overlayStorage) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	localIter, err := o.Storage.IterEncodedObjects(t)
	if err != nil {
		return nil, err
	}
	sharedIter, err := o.shared.IterEncodedObjects(t)
	if err != nil {
		localIter.Close()
		return nil, err
	}
	return &dedupObjectIter{
		iters: []storer.EncodedObjectIter{localIter, sharedIter},
		seen:  make(map[plumbing.Hash]struct{}),
	}, nil
}

type dedupObjectIter struct {
	iters []storer.EncodedObjectIter
	seen  map[plumbing.Hash]struct{}
}

func (d *dedupObjectIter) Next() (plumbing.EncodedObject, error) {
	for len(d.iters) > 0 {
		obj, err := d.iters[0].Next()
		if errors.Is(err, io.EOF) {
			d.iters[0].Close()
			d.iters = d.iters[1:]
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, dup := d.seen[obj.Hash()]; dup {
			continue
		}
		d.seen[obj.Hash()] = struct{}{}
		return obj, nil
	}
	return nil, io.EOF
}

func (d *dedupObjectIter) ForEach(cb func(plumbing.EncodedObject) error) error {
	defer d.Close()
	for {
		obj, err := d.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := cb(obj); err != nil {
			if errors.Is(err, storer.ErrStop) {
				return nil
			}
			return err
		}
	}
}

func (d *dedupObjectIter) Close() {
	for _, it := range d.iters {
		it.Close()
	}
	d.iters = nil
}
