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

package vcs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/go-steer/core-vcs/internal/fakeprovider"
	"github.com/go-steer/core-vcs/internal/gitremote"
	"github.com/go-steer/core-vcs/internal/vcstest"
	"github.com/go-steer/core-vcs/vcs"
)

func repoForRemote(t *testing.T, rem *gitremote.Remote, role vcs.Role, baseBranch string, protected map[string]bool) vcs.Repo {
	t.Helper()
	reg := vcs.NewRegistry()
	reg.MustRegister(fakeprovider.Name, fakeprovider.New(fakeprovider.Options{
		CloneURL:  rem.URL(),
		Protected: protected,
	}))
	forge := vcs.Forge{
		Name:           "f",
		Provider:       fakeprovider.Name,
		Host:           "git.corp.example",
		CAFile:         rem.CAFile,
		CommitIdentity: vcs.Identity{Name: "platform-agent[bot]", Email: "bot@acme.example"},
		Cred:           vcs.StaticToken(fakeprovider.ValidToken),
	}
	cat, err := vcs.NewCatalog(reg, []vcs.Forge{forge}, []vcs.RepoConfig{{
		Forge:      "f",
		Repository: rem.RepoPath,
		Role:       role,
		BaseBranch: baseBranch,
	}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	r, err := cat.Resolve(rem.RepoPath)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return r
}

func TestWorkspaceHappyPathTLSAndAuth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rem := gitremote.NewTLS(t, "acme/infra", map[string]string{
		"deploy/replicas.yaml": "replicas: 2\n",
		"README.md":            "# infra\n",
	})
	rem.RequireAuth(fakeprovider.CloneUser, fakeprovider.ValidToken)

	repo := repoForRemote(t, rem, vcs.RoleGitOps, "main", map[string]bool{"main": true})
	dir := filepath.Join(t.TempDir(), "work")

	ws, err := vcs.Clone(ctx, repo, "", dir, vcs.Policy{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if ws.ClonedBranch() != "main" || ws.ClonedRevision() != rem.BranchHash("main") {
		t.Fatalf("cloned (%s, %s), want (main, %s)", ws.ClonedBranch(), ws.ClonedRevision(), rem.BranchHash("main"))
	}

	st, err := ws.Status()
	if err != nil || !st.Clean || st.Branch != "main" {
		t.Fatalf("Status = %+v, err = %v; want clean on main", st, err)
	}

	// Edit a tracked file and add a new file using ordinary os calls, the
	// way the agent's file tools do.
	if err := os.WriteFile(filepath.Join(dir, "deploy/replicas.yaml"), []byte("replicas: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy/note.txt"), []byte("scaled for peak\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err = ws.Status()
	if err != nil || st.Clean || len(st.Files) != 2 {
		t.Fatalf("Status after edit = %+v, err = %v; want 2 dirty files", st, err)
	}

	diffBefore, err := ws.Diff(vcs.DiffOptions{})
	if err != nil {
		t.Fatalf("Diff before commit: %v", err)
	}
	if !strings.Contains(diffBefore, "-replicas: 2") || !strings.Contains(diffBefore, "+replicas: 3") || !strings.Contains(diffBefore, "+scaled for peak") {
		t.Errorf("Diff before commit missing expected hunks:\n%s", diffBefore)
	}

	if err := ws.Branch("agent/scale-replicas"); err != nil {
		t.Fatalf("Branch: %v", err)
	}
	cm, err := ws.Commit("fix: scale replicas from 2 to 3", vcs.Identity{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if cm.Author.Name != "platform-agent[bot]" {
		t.Errorf("Commit author = %+v, want Forge.CommitIdentity", cm.Author)
	}

	// Against HEAD the tree is now clean; against the cloned base the
	// committed change appears in full.
	if d, err := ws.Diff(vcs.DiffOptions{}); err != nil || d != "" {
		t.Errorf("Diff(HEAD) after commit = %q, %v; want empty", d, err)
	}
	diffBase, err := ws.Diff(vcs.DiffOptions{AgainstBase: true, Paths: []string{"deploy/replicas.yaml"}})
	if err != nil || !strings.Contains(diffBase, "+replicas: 3") || strings.Contains(diffBase, "note.txt") {
		t.Errorf("Diff(AgainstBase, Paths) = %q, %v", diffBase, err)
	}

	logEntries, err := ws.Log(10)
	if err != nil || len(logEntries) != 2 || logEntries[0].Revision != cm.Revision {
		t.Fatalf("Log = %+v, err = %v; want 2 commits with %s first", logEntries, err, cm.Revision)
	}

	pub1, err := ws.Publish(ctx, "")
	if err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if pub1.Branch != "agent/scale-replicas" || pub1.Revision != cm.Revision {
		t.Errorf("Publish = %+v, want agent/scale-replicas @ %s", pub1, cm.Revision)
	}
	if got := rem.BranchHash("agent/scale-replicas"); got != cm.Revision {
		t.Errorf("remote branch hash = %s, want %s", got, cm.Revision)
	}

	// Second round on the same branch fast-forwards our last published tip.
	if err := os.WriteFile(filepath.Join(dir, "deploy/replicas.yaml"), []byte("replicas: 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cm2, err := ws.Commit("fix: bump replicas to 4 after review", vcs.Identity{})
	if err != nil {
		t.Fatalf("second Commit: %v", err)
	}
	pub2, err := ws.Publish(ctx, "agent/scale-replicas")
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if pub2.Revision != cm2.Revision || rem.BranchHash("agent/scale-replicas") != cm2.Revision {
		t.Errorf("second Publish = %+v, remote = %s, want %s", pub2, rem.BranchHash("agent/scale-replicas"), cm2.Revision)
	}
}

func TestGuardCloneTooLarge(t *testing.T) {
	t.Parallel()
	rem := gitremote.New(t, "acme/big", map[string]string{
		"payload.txt": strings.Repeat("x", 256),
	})
	repo := repoForRemote(t, rem, vcs.RoleManaged, "main", nil)
	dir := filepath.Join(t.TempDir(), "work")

	_, err := vcs.Clone(context.Background(), repo, "main", dir, vcs.Policy{MaxCloneBytes: 64})
	vcstest.RequireError(t, err, vcs.CodeCloneTooLarge, "f")
	if !strings.Contains(err.Error(), "64") || !strings.Contains(err.Error(), "256") {
		t.Errorf("CLONE_TOO_LARGE error %q should name both actual bytes (256) and ceiling (64)", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("clone dir %s still exists after CLONE_TOO_LARGE", dir)
	}
}

func TestGuardBaseBranchMissing(t *testing.T) {
	t.Parallel()
	rem := gitremote.New(t, "acme/infra", nil)
	repo := repoForRemote(t, rem, vcs.RoleGitOps, "nonexistent-base", nil)

	_, err := vcs.Clone(context.Background(), repo, "", filepath.Join(t.TempDir(), "work"), vcs.Policy{})
	vcstest.RequireError(t, err, vcs.CodeBaseBranchMissing, "f")
}

func TestGuardWorkspaceRoleContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rem := gitremote.New(t, "acme/ro", map[string]string{"a.txt": "hello\n"})
	repo := repoForRemote(t, rem, vcs.RoleContext, "", nil)
	dir := filepath.Join(t.TempDir(), "work")

	ws, err := vcs.Clone(ctx, repo, "main", dir, vcs.Policy{})
	if err != nil {
		t.Fatalf("Clone on RoleContext: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	vcstest.RequireError(t, ws.Branch("agent/try"), vcs.CodeRoleVCSVerbs, "f")
	_, err = ws.Commit("fix: attempt commit on context repo", vcs.Identity{})
	vcstest.RequireError(t, err, vcs.CodeRoleVCSVerbs, "f")
	_, err = ws.Publish(ctx, "agent/try")
	vcstest.RequireError(t, err, vcs.CodeRoleVCSVerbs, "f")
}

func TestGuardCommitMessageAndEmptyTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rem := gitremote.New(t, "acme/infra", map[string]string{"only.txt": "content\n"})
	repo := repoForRemote(t, rem, vcs.RoleManaged, "main", nil)
	dir := filepath.Join(t.TempDir(), "work")

	ws, err := vcs.Clone(ctx, repo, "main", dir, vcs.Policy{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := ws.Branch("agent/commit-guards"); err != nil {
		t.Fatal(err)
	}

	// Clean working tree -> NOTHING_TO_COMMIT.
	_, err = ws.Commit("fix: nothing changed at all", vcs.Identity{})
	vcstest.RequireError(t, err, vcs.CodeNothingToCommit, "f")

	// Message under 10 chars -> FORGE_REJECTED.
	if err := os.WriteFile(filepath.Join(dir, "only.txt"), []byte("updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ws.Commit("too short", vcs.Identity{})
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")

	// Removing every tracked file leaves an empty tree -> NOTHING_TO_COMMIT.
	if err := os.Remove(filepath.Join(dir, "only.txt")); err != nil {
		t.Fatal(err)
	}
	_, err = ws.Commit("fix: delete the only file in the repo", vcs.Identity{})
	vcstest.RequireError(t, err, vcs.CodeNothingToCommit, "f")
}

func TestGuardTargetIsBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rem := gitremote.New(t, "acme/infra", map[string]string{"f.txt": "v1\n"})
	repo := repoForRemote(t, rem, vcs.RoleGitOps, "main", map[string]bool{"main": false})
	dir := filepath.Join(t.TempDir(), "work")

	ws, err := vcs.Clone(ctx, repo, "main", dir, vcs.Policy{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Commit("fix: commit directly on main", vcs.Identity{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Even with main marked unprotected on the forge, the default
	// clone -> edit -> commit -> publish sequence on the base branch is
	// refused with TARGET_IS_BRANCH before any network call.
	vcstest.RequireError(t, ws.Branch("main"), vcs.CodeTargetIsBranch, "f")
	_, err = ws.Publish(ctx, "")
	vcstest.RequireError(t, err, vcs.CodeTargetIsBranch, "f")
	_, err = ws.Publish(ctx, "main")
	vcstest.RequireError(t, err, vcs.CodeTargetIsBranch, "f")
}

func TestGuardBranchNotOurs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rem := gitremote.New(t, "acme/infra", map[string]string{"f.txt": "v1\n"})
	repo := repoForRemote(t, rem, vcs.RoleGitOps, "main", nil)
	dir := filepath.Join(t.TempDir(), "work")

	ws, err := vcs.Clone(ctx, repo, "main", dir, vcs.Policy{BranchPrefix: "agent/"})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cm, err := ws.Commit("fix: local commit before branching", vcs.Identity{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// 1. Lacks BranchPrefix -> BRANCH_NOT_OURS on both Branch and Publish.
	vcstest.RequireError(t, ws.Branch("feature/no-prefix"), vcs.CodeBranchNotOurs, "f")
	_, err = ws.Publish(ctx, "feature/no-prefix")
	vcstest.RequireError(t, err, vcs.CodeBranchNotOurs, "f")

	// 2. Has BranchPrefix and exists in .git/refs/heads/, but was NOT
	// created via Workspace.Branch in this process (simulates a model
	// writing .git/refs/heads/agent/forged on disk).
	rawRepo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	forgedRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("agent/forged"), plumbing.NewHash(cm.Revision))
	if err := rawRepo.Storer.SetReference(forgedRef); err != nil {
		t.Fatal(err)
	}
	_, err = ws.Publish(ctx, "agent/forged")
	vcstest.RequireError(t, err, vcs.CodeBranchNotOurs, "f")
}

func TestGuardBaseMovedDirection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Part 1: Target advances normally after Clone -> first Publish MUST
	// SUCCEED (base-under-target direction: clonedRevision is still an
	// ancestor of remote main).
	remAdvance := gitremote.New(t, "acme/infra", map[string]string{"a.txt": "v1\n"})
	repoAdvance := repoForRemote(t, remAdvance, vcs.RoleGitOps, "main", nil)
	dirAdvance := filepath.Join(t.TempDir(), "advance")

	wsAdvance, err := vcs.Clone(ctx, repoAdvance, "main", dirAdvance, vcs.Policy{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := wsAdvance.Branch("agent/concurrent-ok"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirAdvance, "b.txt"), []byte("topic work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wsAdvance.Commit("feat: add b.txt on topic branch", vcs.Identity{}); err != nil {
		t.Fatal(err)
	}
	// Someone else lands a commit on main while our copy is open.
	remAdvance.CommitOnBranch(t, "main", "other team commit on main", map[string]string{"other.txt": "main advanced\n"}, false)

	if _, err := wsAdvance.Publish(ctx, "agent/concurrent-ok"); err != nil {
		t.Fatalf("Publish after ordinary advance on main failed: %v (base-under-target must allow ordinary advance)", err)
	}

	// Part 2: Target is rewritten (force-pushed to an unrelated root)
	// after Clone -> first Publish MUST FAIL with BASE_MOVED.
	remRewrite := gitremote.New(t, "acme/infra", map[string]string{"a.txt": "v1\n"})
	repoRewrite := repoForRemote(t, remRewrite, vcs.RoleGitOps, "main", nil)
	dirRewrite := filepath.Join(t.TempDir(), "rewrite")

	wsRewrite, err := vcs.Clone(ctx, repoRewrite, "main", dirRewrite, vcs.Policy{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := wsRewrite.Branch("agent/on-rewritten-base"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirRewrite, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wsRewrite.Commit("fix: update a.txt", vcs.Identity{}); err != nil {
		t.Fatal(err)
	}
	// Rewrite main to an orphan commit so clonedRevision is no longer an
	// ancestor of main.
	remRewrite.CommitOnBranch(t, "main", "rewritten root", map[string]string{"a.txt": "rewritten\n"}, true)

	_, err = wsRewrite.Publish(ctx, "agent/on-rewritten-base")
	vcstest.RequireError(t, err, vcs.CodeBaseMoved, "f")

	// Part 3: BASE_MOVED is asked on first publish ONLY. A second publish
	// on an already-published branch succeeds even if main was rewritten
	// afterwards.
	remAdvance.CommitOnBranch(t, "main", "rewrite main after first publish", map[string]string{"a.txt": "rewritten\n"}, true)
	if err := os.WriteFile(filepath.Join(dirAdvance, "b.txt"), []byte("topic work round 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wsAdvance.Commit("feat: second round on topic branch", vcs.Identity{}); err != nil {
		t.Fatal(err)
	}
	if _, err := wsAdvance.Publish(ctx, "agent/concurrent-ok"); err != nil {
		t.Fatalf("second Publish after main rewrite failed: %v (BASE_MOVED must run on first publish only)", err)
	}
}

func TestGuardBranchDivergedAndBranchMoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rem := gitremote.New(t, "acme/infra", map[string]string{"a.txt": "v1\n"})
	// Pre-existing remote branch under agent/ that this Workspace did not publish.
	rem.CommitOnBranch(t, "agent/taken", "prior run", map[string]string{"a.txt": "prior\n"}, false)

	repo := repoForRemote(t, rem, vcs.RoleGitOps, "main", nil)
	dir := filepath.Join(t.TempDir(), "work")
	ws, err := vcs.Clone(ctx, repo, "main", dir, vcs.Policy{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}

	// First publish to a name that already exists on the remote -> BRANCH_DIVERGED.
	if err := ws.Branch("agent/taken"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Commit("fix: change on taken branch", vcs.Identity{}); err != nil {
		t.Fatal(err)
	}
	_, err = ws.Publish(ctx, "agent/taken")
	vcstest.RequireError(t, err, vcs.CodeBranchDiverged, "f")

	// Publish to a fresh branch succeeds, then an out-of-band update to
	// that branch on the remote causes the second publish to fail with
	// BRANCH_MOVED.
	if err := ws.Branch("agent/fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Publish(ctx, "agent/fresh"); err != nil {
		t.Fatalf("first Publish(agent/fresh): %v", err)
	}
	rem.CommitOnBranch(t, "agent/fresh", "out-of-band commit", map[string]string{"oob.txt": "1\n"}, false)

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Commit("fix: second commit on fresh", vcs.Identity{}); err != nil {
		t.Fatal(err)
	}
	_, err = ws.Publish(ctx, "agent/fresh")
	vcstest.RequireError(t, err, vcs.CodeBranchMoved, "f")

	// Non-fast-forward of our own last published tip: publish agent/ff-test,
	// then rewind its local ref back to ClonedRevision and try to publish
	// an alternate commit that does not descend from the first published tip.
	if err := ws.Branch("agent/ff-test"); err != nil {
		t.Fatal(err)
	}
	pub1, err := ws.Publish(ctx, "agent/ff-test")
	if err != nil {
		t.Fatalf("first Publish(agent/ff-test): %v", err)
	}
	rawRepo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	rewind := plumbing.NewHashReference(plumbing.NewBranchReferenceName("agent/ff-test"), plumbing.NewHash(ws.ClonedRevision()))
	if err := rawRepo.Storer.SetReference(rewind); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("diverged-local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmDiv, err := ws.Commit("fix: commit after local rewind", vcs.Identity{})
	if err != nil {
		t.Fatal(err)
	}
	if cmDiv.Revision == pub1.Revision {
		t.Fatal("expected distinct commit after rewind")
	}
	_, err = ws.Publish(ctx, "agent/ff-test")
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")
}

func TestGuardProtectedBranchAndPushRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rem := gitremote.New(t, "acme/infra", map[string]string{"a.txt": "v1\n"})
	repo := repoForRemote(t, rem, vcs.RoleGitOps, "main", map[string]bool{
		"main":                 true,
		"agent/protected-rule": true,
	})
	dir := filepath.Join(t.TempDir(), "work")
	ws, err := vcs.Clone(ctx, repo, "main", dir, vcs.Policy{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := ws.Branch("agent/protected-rule"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Commit("fix: test protected topic branch", vcs.Identity{}); err != nil {
		t.Fatal(err)
	}

	_, err = ws.Publish(ctx, "agent/protected-rule")
	vcstest.RequireError(t, err, vcs.CodeProtectedBranch, "f")

	// Unprotected branch when the server rejects the receive-pack -> FORGE_REJECTED.
	if err := ws.Branch("agent/unprotected"); err != nil {
		t.Fatal(err)
	}
	rem.RejectNextPush("pre-receive hook declined: policy check failed")
	_, err = ws.Publish(ctx, "agent/unprotected")
	vcstest.RequireError(t, err, vcs.CodeForgeRejected, "f")
}

func TestGitConfigPoisoningIsIgnored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rem := gitremote.New(t, "acme/infra", map[string]string{"a.txt": "v1\n"})
	repo := repoForRemote(t, rem, vcs.RoleGitOps, "main", nil)
	dir := filepath.Join(t.TempDir(), "work")

	ws, err := vcs.Clone(ctx, repo, "main", dir, vcs.Policy{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	// Poison .git/config with a bogus origin URL. Publish must ignore
	// .git/config and push to repo.CloneURL() held in memory.
	gitConfig := filepath.Join(dir, ".git", "config")
	poison := "[core]\n\trepositoryformatversion = 0\n\tbare = false\n[remote \"origin\"]\n\turl = https://127.0.0.1:1/exfil.git\n"
	if err := os.WriteFile(gitConfig, []byte(poison), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ws.Branch("agent/safe-push"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cm, err := ws.Commit("fix: push despite poisoned .git/config", vcs.Identity{})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ws.Publish(ctx, "agent/safe-push")
	if err != nil {
		t.Fatalf("Publish failed after .git/config edit: %v", err)
	}
	if rem.BranchHash("agent/safe-push") != cm.Revision || pub.Revision != cm.Revision {
		t.Fatalf("remote hash = %s, want %s", rem.BranchHash("agent/safe-push"), cm.Revision)
	}
}
