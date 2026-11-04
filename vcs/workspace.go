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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

const (
	// DefaultMaxCloneBytes is the default working-tree size ceiling (256 MiB).
	DefaultMaxCloneBytes int64 = 256 << 20
	// DefaultBranchPrefix is the default prefix every branch created by a
	// Workspace must start with.
	DefaultBranchPrefix = "agent/"
	// minCommitMessageRunes is the shortest commit message Commit accepts.
	minCommitMessageRunes = 10
)

// Policy holds the install-wide workspace rules. Zero values select
// DefaultMaxCloneBytes and DefaultBranchPrefix.
type Policy struct {
	// MaxCloneBytes caps the total size of regular files in the checked-out
	// working tree (excluding .git/). 0 means DefaultMaxCloneBytes.
	MaxCloneBytes int64
	// BranchPrefix is the required prefix on every branch Branch creates
	// and Publish pushes. "" means DefaultBranchPrefix.
	BranchPrefix string
}

func (p Policy) normalize(forge string) (Policy, error) {
	if p.MaxCloneBytes < 0 {
		return Policy{}, Errorf(CodeConfigInvalid, forge, "MaxCloneBytes %d must not be negative", p.MaxCloneBytes)
	}
	if p.MaxCloneBytes == 0 {
		p.MaxCloneBytes = DefaultMaxCloneBytes
	}
	if p.BranchPrefix == "" {
		p.BranchPrefix = DefaultBranchPrefix
	}
	if err := checkBranchName(forge, p.BranchPrefix+"probe"); err != nil {
		return Policy{}, Errorf(CodeConfigInvalid, forge, "invalid BranchPrefix %q: %w", p.BranchPrefix, err)
	}
	return p, nil
}

// Workspace is a go-git working copy of one Repo.
//
// There is no Open(dir) constructor: a Workspace exists only as the
// return value of Clone in this process. It holds the cloned base branch,
// the cloned revision and the set of branches it created in memory, never
// on disk, so nothing inside the working directory — .git/ included — is
// trusted as a record of what this process did.
type Workspace struct {
	repo   Repo
	dir    string
	policy Policy
	git    *git.Repository

	clonedBranch   string
	clonedRevision string

	mu   sync.Mutex
	ours map[string]*branchRecord
}

type branchRecord struct {
	base          string
	lastPublished string
}

// FileStatus is one modified, added, deleted or untracked path in the
// working copy.
type FileStatus struct {
	Path     string
	Staging  string
	Worktree string
}

// Status is the local state of a Workspace.
type Status struct {
	Branch   string
	Revision string
	Clean    bool
	Files    []FileStatus
}

// DiffOptions controls Workspace.Diff.
type DiffOptions struct {
	// AgainstBase diffs the working tree against the revision Clone
	// checked out rather than against HEAD, so committed and uncommitted
	// changes on a topic branch appear together.
	AgainstBase bool
	// Paths restricts the diff to the named repository-relative paths when
	// non-empty.
	Paths []string
}

// PublishResult is what Workspace.Publish returns after a push succeeds.
type PublishResult struct {
	Branch   string
	Revision string
}

// Clone materialises a single-branch, non-shallow working copy of repo at
// dir. When branch is empty, Clone checks out repo.BaseBranchName.
//
// If the checked-out working tree exceeds policy.MaxCloneBytes, dir is
// removed and Clone returns CLONE_TOO_LARGE naming the ceiling.
func Clone(ctx context.Context, repo Repo, branch, dir string, policy Policy) (*Workspace, error) {
	if err := repo.ready(VerbClone); err != nil {
		return nil, err
	}
	pol, err := policy.normalize(repo.Forge.Name)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(dir) == "" {
		return nil, Errorf(CodeConfigInvalid, repo.Forge.Name, "clone directory is empty").WithVerb(VerbClone)
	}
	branch = strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
	if branch == "" {
		branch, err = repo.BaseBranchName(ctx)
		if err != nil {
			return nil, repo.wrap(VerbClone, err)
		}
	}
	gitAuth, caBundle, err := transportCreds(ctx, repo, VerbClone)
	if err != nil {
		return nil, err
	}

	gitRepo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:           repo.CloneURL(),
		Auth:          gitAuth,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
		Tags:          git.NoTags,
		CABundle:      caBundle,
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, classifyCloneError(repo, branch, err)
	}

	// Remove the on-disk remote so .git/config carries no remote URL;
	// Publish always dials repo.CloneURL() from memory.
	_ = gitRepo.DeleteRemote(git.DefaultRemoteName)

	treeBytes, err := measureWorktree(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, Errorf(CodeGitFailed, repo.Forge.Name, "measuring working tree: %w", err).WithVerb(VerbClone)
	}
	if treeBytes > pol.MaxCloneBytes {
		_ = os.RemoveAll(dir)
		return nil, Errorf(
			CodeCloneTooLarge,
			repo.Forge.Name,
			"working tree is %d bytes, exceeding MaxCloneBytes ceiling of %d bytes",
			treeBytes, pol.MaxCloneBytes,
		).WithVerb(VerbClone)
	}

	head, err := gitRepo.Head()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, Errorf(CodeGitFailed, repo.Forge.Name, "reading HEAD after clone: %w", err).WithVerb(VerbClone)
	}
	return &Workspace{
		repo:           repo,
		dir:            dir,
		policy:         pol,
		git:            gitRepo,
		clonedBranch:   branch,
		clonedRevision: head.Hash().String(),
		ours:           map[string]*branchRecord{},
	}, nil
}

// Repo returns the Repo this Workspace was cloned from.
func (w *Workspace) Repo() Repo { return w.repo }

// Dir returns the working-copy directory.
func (w *Workspace) Dir() string { return w.dir }

// ClonedBranch returns the base branch Clone checked out.
func (w *Workspace) ClonedBranch() string { return w.clonedBranch }

// ClonedRevision returns the commit hash of ClonedBranch at Clone time.
func (w *Workspace) ClonedRevision() string { return w.clonedRevision }

// CurrentBranch returns the short name of the currently checked-out branch.
func (w *Workspace) CurrentBranch() (string, error) {
	head, err := w.git.Head()
	if err != nil {
		return "", Errorf(CodeGitFailed, w.repo.Forge.Name, "reading HEAD: %w", err)
	}
	if !head.Name().IsBranch() {
		return "", Errorf(CodeGitFailed, w.repo.Forge.Name, "HEAD (%s) is detached", head.Name())
	}
	return head.Name().Short(), nil
}

// HEAD returns the current commit hash.
func (w *Workspace) HEAD() (string, error) {
	head, err := w.git.Head()
	if err != nil {
		return "", Errorf(CodeGitFailed, w.repo.Forge.Name, "reading HEAD: %w", err)
	}
	return head.Hash().String(), nil
}

// Status reports the current branch, HEAD revision and modified/untracked
// files in the working copy. No network.
func (w *Workspace) Status() (Status, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	head, err := w.git.Head()
	if err != nil {
		return Status{}, Errorf(CodeGitFailed, w.repo.Forge.Name, "reading HEAD: %w", err)
	}
	wt, err := w.git.Worktree()
	if err != nil {
		return Status{}, Errorf(CodeGitFailed, w.repo.Forge.Name, "opening worktree: %w", err)
	}
	st, err := wt.Status()
	if err != nil {
		return Status{}, Errorf(CodeGitFailed, w.repo.Forge.Name, "worktree status: %w", err)
	}
	var files []FileStatus
	for path, fs := range st {
		if fs.Staging == git.Unmodified && fs.Worktree == git.Unmodified {
			continue
		}
		files = append(files, FileStatus{
			Path:     path,
			Staging:  string(fs.Staging),
			Worktree: string(fs.Worktree),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return Status{
		Branch:   head.Name().Short(),
		Revision: head.Hash().String(),
		Clean:    len(files) == 0,
		Files:    files,
	}, nil
}

// Log returns up to limit commits reachable from HEAD (default 20 when
// limit <= 0), newest first. No network.
func (w *Workspace) Log(limit int) ([]Commit, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if limit <= 0 {
		limit = 20
	}
	head, err := w.git.Head()
	if err != nil {
		return nil, Errorf(CodeGitFailed, w.repo.Forge.Name, "reading HEAD: %w", err)
	}
	iter, err := w.git.Log(&git.LogOptions{
		From:  head.Hash(),
		Order: git.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, Errorf(CodeGitFailed, w.repo.Forge.Name, "git log: %w", err)
	}
	defer iter.Close()

	var out []Commit
	for len(out) < limit {
		c, err := iter.Next()
		if err != nil {
			break
		}
		out = append(out, Commit{
			Revision: c.Hash.String(),
			Message:  strings.TrimRight(c.Message, "\n"),
			Author:   Identity{Name: c.Author.Name, Email: c.Author.Email},
			When:     c.Author.When,
		})
	}
	return out, nil
}

// Branch creates (or switches to, if already created by this Workspace) a
// topic branch under Policy.BranchPrefix and records it in memory as ours.
func (w *Workspace) Branch(name string) error {
	if err := w.repo.ready(VerbPublish); err != nil {
		return err
	}
	name = strings.TrimPrefix(strings.TrimSpace(name), "refs/heads/")
	if name == w.clonedBranch || (w.repo.BaseBranch != "" && name == w.repo.BaseBranch) {
		return Errorf(CodeTargetIsBranch, w.repo.Forge.Name, "branch %q is the base branch; pick a name under %q", name, w.policy.BranchPrefix)
	}
	if !strings.HasPrefix(name, w.policy.BranchPrefix) || len(name) <= len(w.policy.BranchPrefix) {
		return Errorf(CodeBranchNotOurs, w.repo.Forge.Name, "branch %q must start with prefix %q", name, w.policy.BranchPrefix)
	}
	if err := checkBranchName(w.repo.Forge.Name, name); err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	head, err := w.git.Head()
	if err != nil {
		return Errorf(CodeGitFailed, w.repo.Forge.Name, "reading HEAD: %w", err)
	}
	wt, err := w.git.Worktree()
	if err != nil {
		return Errorf(CodeGitFailed, w.repo.Forge.Name, "opening worktree: %w", err)
	}
	_, alreadyOurs := w.ours[name]
	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(name),
		Create: !alreadyOurs,
		Keep:   true,
	}); err != nil {
		return Errorf(CodeGitFailed, w.repo.Forge.Name, "checking out branch %q: %w", name, err)
	}
	if !alreadyOurs {
		w.ours[name] = &branchRecord{base: head.Hash().String()}
	}
	return nil
}

// Commit stages all working-tree changes and records a commit. It refuses
// a RoleContext repository (ROLE_VCS_VERBS), a message under 10 characters
// (FORGE_REJECTED), a clean working tree (NOTHING_TO_COMMIT), and a commit
// whose resulting tree would be empty (NOTHING_TO_COMMIT).
//
// Author and committer are taken from identity when non-empty, else from
// Forge.CommitIdentity.
func (w *Workspace) Commit(msg string, identity Identity) (Commit, error) {
	if err := w.repo.ready(VerbCommit); err != nil {
		return Commit{}, err
	}
	msg = strings.TrimSpace(msg)
	if utf8.RuneCountInString(msg) < minCommitMessageRunes {
		return Commit{}, Errorf(CodeForgeRejected, w.repo.Forge.Name, "commit message %q must be at least %d characters", msg, minCommitMessageRunes).WithVerb(VerbCommit)
	}
	if identity.Name == "" {
		identity.Name = w.repo.Forge.CommitIdentity.Name
	}
	if identity.Email == "" {
		identity.Email = w.repo.Forge.CommitIdentity.Email
	}
	if strings.TrimSpace(identity.Name) == "" || strings.TrimSpace(identity.Email) == "" {
		return Commit{}, Errorf(CodeConfigInvalid, w.repo.Forge.Name, "commit author Name and Email are required; set Forge.CommitIdentity or pass an Identity").WithVerb(VerbCommit)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	wt, err := w.git.Worktree()
	if err != nil {
		return Commit{}, Errorf(CodeGitFailed, w.repo.Forge.Name, "opening worktree: %w", err).WithVerb(VerbCommit)
	}
	st, err := wt.Status()
	if err != nil {
		return Commit{}, Errorf(CodeGitFailed, w.repo.Forge.Name, "worktree status: %w", err).WithVerb(VerbCommit)
	}
	if st.IsClean() {
		return Commit{}, Errorf(CodeNothingToCommit, w.repo.Forge.Name, "working tree has no changes to commit").WithVerb(VerbCommit)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return Commit{}, Errorf(CodeGitFailed, w.repo.Forge.Name, "staging changes: %w", err).WithVerb(VerbCommit)
	}
	idx, err := w.git.Storer.Index()
	if err != nil {
		return Commit{}, Errorf(CodeGitFailed, w.repo.Forge.Name, "reading index: %w", err).WithVerb(VerbCommit)
	}
	if len(idx.Entries) == 0 {
		return Commit{}, Errorf(CodeNothingToCommit, w.repo.Forge.Name, "refusing to commit an empty tree").WithVerb(VerbCommit)
	}

	now := time.Now().UTC()
	sig := &object.Signature{Name: identity.Name, Email: identity.Email, When: now}
	hash, err := wt.Commit(msg, &git.CommitOptions{
		Author:    sig,
		Committer: sig,
	})
	if err != nil {
		if errors.Is(err, git.ErrEmptyCommit) {
			return Commit{}, Errorf(CodeNothingToCommit, w.repo.Forge.Name, "no changes to commit: %w", err).WithVerb(VerbCommit)
		}
		return Commit{}, Errorf(CodeGitFailed, w.repo.Forge.Name, "commit: %w", err).WithVerb(VerbCommit)
	}
	return Commit{
		Revision: hash.String(),
		Message:  msg,
		Author:   identity,
		When:     now,
	}, nil
}

// Publish pushes branch to the remote after checking every guard:
//
//  1. ROLE_VCS_VERBS (before any network) if the repository's role is context.
//  2. TARGET_IS_BRANCH (before any network) if branch is the cloned base or
//     the configured BaseBranch.
//  3. BRANCH_NOT_OURS (before any network) if branch lacks BranchPrefix or
//     was not created by this Workspace in this process.
//  4. BASE_MOVED (first publish only, base-under-target direction) if the
//     remote's target branch no longer has ClonedRevision as an ancestor.
//  5. BRANCH_DIVERGED (first publish) if the branch already exists on the
//     remote, or BRANCH_MOVED (subsequent publish) if the remote tip is not
//     our last published tip.
//  6. PROTECTED_BRANCH via the provider before pushing.
//  7. Fast-forward push of our own last tip only; there is no force-push.
//
// When branch is empty, Publish uses CurrentBranch.
func (w *Workspace) Publish(ctx context.Context, branch string) (PublishResult, error) {
	if err := w.repo.ready(VerbPublish); err != nil {
		return PublishResult{}, err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if strings.TrimSpace(branch) == "" {
		cur, err := w.CurrentBranch()
		if err != nil {
			return PublishResult{}, err
		}
		branch = cur
	}
	branch = strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")

	// Guard 1: TARGET_IS_BRANCH (before any network).
	if branch == w.clonedBranch || (w.repo.BaseBranch != "" && branch == w.repo.BaseBranch) {
		return PublishResult{}, Errorf(
			CodeTargetIsBranch,
			w.repo.Forge.Name,
			"branch %q is the target branch; create a topic branch under %q with Workspace.Branch before publishing",
			branch, w.policy.BranchPrefix,
		).WithVerb(VerbPublish)
	}

	// Guard 2: BRANCH_NOT_OURS — prefix and in-process creation (before any network).
	if !strings.HasPrefix(branch, w.policy.BranchPrefix) || len(branch) <= len(w.policy.BranchPrefix) {
		return PublishResult{}, Errorf(
			CodeBranchNotOurs,
			w.repo.Forge.Name,
			"branch %q does not start with required prefix %q; create a branch with Workspace.Branch",
			branch, w.policy.BranchPrefix,
		).WithVerb(VerbPublish)
	}
	rec, ok := w.ours[branch]
	if !ok {
		return PublishResult{}, Errorf(
			CodeBranchNotOurs,
			w.repo.Forge.Name,
			"branch %q was not created by this Workspace in this process; clone again and create the branch with Workspace.Branch",
			branch,
		).WithVerb(VerbPublish)
	}

	branchRef := plumbing.NewBranchReferenceName(branch)
	localRef, err := w.git.Reference(branchRef, true)
	if err != nil {
		return PublishResult{}, Errorf(CodeBranchNotOurs, w.repo.Forge.Name, "branch %q does not exist locally: %w", branch, err).WithVerb(VerbPublish)
	}
	localTip, err := w.git.CommitObject(localRef.Hash())
	if err != nil {
		return PublishResult{}, Errorf(CodeGitFailed, w.repo.Forge.Name, "loading commit %s: %w", localRef.Hash(), err).WithVerb(VerbPublish)
	}
	clonedCommit, err := w.git.CommitObject(plumbing.NewHash(w.clonedRevision))
	if err != nil {
		return PublishResult{}, Errorf(CodeGitFailed, w.repo.Forge.Name, "loading cloned base %s: %w", w.clonedRevision, err).WithVerb(VerbPublish)
	}
	if desc, err := clonedCommit.IsAncestor(localTip); err != nil || !desc {
		return PublishResult{}, Errorf(
			CodeBranchNotOurs,
			w.repo.Forge.Name,
			"branch %q (%s) does not descend from cloned base %s; clone again",
			branch, localTip.Hash, w.clonedRevision,
		).WithVerb(VerbPublish)
	}

	gitAuth, caBundle, err := transportCreds(ctx, w.repo, VerbPublish)
	if err != nil {
		return PublishResult{}, err
	}
	// Construct the remote in memory from w.repo.CloneURL(); never read
	// .git/config from disk.
	rem := git.NewRemote(w.git.Storer, &config.RemoteConfig{
		Name: git.DefaultRemoteName,
		URLs: []string{w.repo.CloneURL()},
	})
	remoteRefs, err := rem.ListContext(ctx, &git.ListOptions{
		Auth:     gitAuth,
		CABundle: caBundle,
	})
	if err != nil {
		return PublishResult{}, classifyRemoteError(w.repo, VerbPublish, err)
	}
	refMap := map[plumbing.ReferenceName]plumbing.Hash{}
	for _, r := range remoteRefs {
		if r.Type() == plumbing.HashReference {
			refMap[r.Name()] = r.Hash()
		}
	}

	// Guard 3: BASE_MOVED (first publish only, base-under-target direction).
	if rec.lastPublished == "" {
		if err := w.checkBaseMoved(ctx, rem, gitAuth, caBundle, clonedCommit, refMap); err != nil {
			return PublishResult{}, err
		}
		if existing, exists := refMap[branchRef]; exists {
			return PublishResult{}, Errorf(
				CodeBranchDiverged,
				w.repo.Forge.Name,
				"remote branch %q already exists at %s and was not published by this Workspace; create a new branch name",
				branch, existing,
			).WithVerb(VerbPublish)
		}
	} else {
		remoteBranchHash, exists := refMap[branchRef]
		if !exists || remoteBranchHash.String() != rec.lastPublished {
			return PublishResult{}, Errorf(
				CodeBranchMoved,
				w.repo.Forge.Name,
				"remote branch %q is at %s, expected our last published tip %s; someone else updated the branch, clone again",
				branch, remoteBranchHash, rec.lastPublished,
			).WithVerb(VerbPublish)
		}
		prevCommit, err := w.git.CommitObject(plumbing.NewHash(rec.lastPublished))
		if err != nil {
			return PublishResult{}, Errorf(CodeGitFailed, w.repo.Forge.Name, "loading last published commit %s: %w", rec.lastPublished, err).WithVerb(VerbPublish)
		}
		if ff, err := prevCommit.IsAncestor(localTip); err != nil || !ff {
			return PublishResult{}, Errorf(
				CodeForgeRejected,
				w.repo.Forge.Name,
				"branch %q (%s) is not a fast-forward of last published tip %s; force-push is not supported, commit on top of %s",
				branch, localTip.Hash, rec.lastPublished, rec.lastPublished,
			).WithVerb(VerbPublish)
		}
	}

	// Guard 4: PROTECTED_BRANCH via the provider before pushing.
	protected, err := w.repo.ProtectedBranch(ctx, branch)
	if err != nil {
		return PublishResult{}, err
	}
	if protected {
		return PublishResult{}, Errorf(
			CodeProtectedBranch,
			w.repo.Forge.Name,
			"branch %q is protected on forge %q; publish to an unprotected topic branch under %q",
			branch, w.repo.Forge.Name, w.policy.BranchPrefix,
		).WithVerb(VerbPublish)
	}

	var requireRemote []config.RefSpec
	if rec.lastPublished != "" {
		requireRemote = []config.RefSpec{config.RefSpec(fmt.Sprintf("%s:%s", rec.lastPublished, branchRef))}
	}
	spec := config.RefSpec(fmt.Sprintf("%s:%s", branchRef, branchRef))
	err = rem.PushContext(ctx, &git.PushOptions{
		RemoteName:        git.DefaultRemoteName,
		RefSpecs:          []config.RefSpec{spec},
		RequireRemoteRefs: requireRemote,
		Auth:              gitAuth,
		CABundle:          caBundle,
		Force:             false,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return PublishResult{}, classifyPushError(w.repo, err)
	}

	rec.lastPublished = localTip.Hash.String()
	return PublishResult{
		Branch:   branch,
		Revision: localTip.Hash.String(),
	}, nil
}

func (w *Workspace) checkBaseMoved(
	ctx context.Context,
	rem *git.Remote,
	gitAuth transport.AuthMethod,
	caBundle []byte,
	clonedCommit *object.Commit,
	refMap map[plumbing.ReferenceName]plumbing.Hash,
) error {
	targetRef := plumbing.NewBranchReferenceName(w.clonedBranch)
	remoteTargetHash, ok := refMap[targetRef]
	if !ok {
		return Errorf(
			CodeBaseMoved,
			w.repo.Forge.Name,
			"target branch %q no longer exists on the remote; clone again",
			w.clonedBranch,
		).WithVerb(VerbPublish)
	}
	if remoteTargetHash.String() == w.clonedRevision {
		return nil
	}

	// Target moved on the remote since Clone. Fetch its tip into a scratch
	// tracking ref and check whether ClonedRevision is still an ancestor
	// of it (base-under-target direction: an ordinary advance passes; a
	// rewritten history fails BASE_MOVED).
	trackingRef := plumbing.ReferenceName("refs/remotes/vcs/target")
	spec := config.RefSpec(fmt.Sprintf("+%s:%s", targetRef, trackingRef))
	err := rem.FetchContext(ctx, &git.FetchOptions{
		RemoteName: git.DefaultRemoteName,
		RefSpecs:   []config.RefSpec{spec},
		Auth:       gitAuth,
		CABundle:   caBundle,
		Tags:       git.NoTags,
		Force:      true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return classifyRemoteError(w.repo, VerbPublish, err)
	}
	remoteTargetCommit, err := w.git.CommitObject(remoteTargetHash)
	if err != nil {
		return Errorf(CodeBaseMoved, w.repo.Forge.Name, "loading remote target %s: %w; clone again", remoteTargetHash, err).WithVerb(VerbPublish)
	}
	anc, err := clonedCommit.IsAncestor(remoteTargetCommit)
	if err != nil || !anc {
		return Errorf(
			CodeBaseMoved,
			w.repo.Forge.Name,
			"target branch %q was rewritten since clone (cloned revision %s is no longer an ancestor of remote tip %s); clone again",
			w.clonedBranch, w.clonedRevision, remoteTargetHash,
		).WithVerb(VerbPublish)
	}
	return nil
}

func transportCreds(ctx context.Context, repo Repo, verb Verb) (transport.AuthMethod, []byte, error) {
	if _, err := TLSConfig(repo.Forge); err != nil {
		return nil, nil, repo.wrap(verb, err)
	}
	var caBundle []byte
	if repo.Forge.CAFile != "" {
		b, err := os.ReadFile(repo.Forge.CAFile)
		if err != nil {
			return nil, nil, Errorf(CodeForgeTLSUntrusted, repo.Forge.Name, "CAFile %s: %w", repo.Forge.CAFile, err).WithVerb(verb)
		}
		caBundle = b
	}
	auth, err := repo.CloneAuth(ctx)
	if err != nil {
		return nil, nil, repo.wrap(verb, err)
	}
	if auth == nil {
		return nil, caBundle, nil
	}
	return &githttp.BasicAuth{Username: auth.Username, Password: auth.Password}, caBundle, nil
}

func classifyCloneError(repo Repo, branch string, err error) error {
	msg := err.Error()
	if errors.Is(err, plumbing.ErrReferenceNotFound) ||
		errors.Is(err, transport.ErrEmptyRemoteRepository) ||
		strings.Contains(msg, "reference not found") ||
		strings.Contains(msg, "couldn't find remote ref") {
		return Errorf(
			CodeBaseBranchMissing,
			repo.Forge.Name,
			"branch %q does not exist on remote %s: %w",
			branch, repo.Ref, err,
		).WithVerb(VerbClone)
	}
	return classifyRemoteError(repo, VerbClone, err)
}

func classifyRemoteError(repo Repo, verb Verb, err error) error {
	if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) {
		return Errorf(CodeForgeUnauthorized, repo.Forge.Name, "%w; check the forge credential, do not retry", err).WithVerb(verb)
	}
	if isTLSError(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return TransportError(repo.Forge, verb, err)
	}
	return Errorf(CodeGitFailed, repo.Forge.Name, "%w", err).WithVerb(verb)
}

func classifyPushError(repo Repo, err error) error {
	if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) {
		msg := err.Error()
		if strings.Contains(msg, "authorization failed") && !strings.Contains(msg, "authentication required") {
			return Errorf(CodeForgeRejected, repo.Forge.Name, "push rejected by remote: %w", err).WithVerb(VerbPublish)
		}
		return Errorf(CodeForgeUnauthorized, repo.Forge.Name, "%w; check the forge credential, do not retry", err).WithVerb(VerbPublish)
	}
	msg := err.Error()
	if strings.Contains(msg, "required to be") {
		return Errorf(CodeBranchMoved, repo.Forge.Name, "remote branch moved during publish: %w; clone again", err).WithVerb(VerbPublish)
	}
	if strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "failed to update ref") ||
		strings.Contains(msg, "command error on") ||
		strings.Contains(msg, "hook declined") ||
		strings.Contains(msg, "protected") ||
		strings.Contains(msg, "rejected") {
		return Errorf(CodeForgeRejected, repo.Forge.Name, "push rejected by remote: %w", err).WithVerb(VerbPublish)
	}
	return classifyRemoteError(repo, VerbPublish, err)
}

func measureWorktree(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func checkBranchName(forge, name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.HasPrefix(name, "-") || strings.HasSuffix(name, ".lock") ||
		strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return Errorf(CodeForgeRejected, forge, "invalid branch name %q", name)
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(`\:?*[~^`, r) {
			return Errorf(CodeForgeRejected, forge, "invalid character %q in branch name %q", r, name)
		}
	}
	return nil
}
