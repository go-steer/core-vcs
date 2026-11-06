---
title: Getting started
description: Configure a Catalog, clone a repository with a shared cache, commit, publish, and open a proposal.
sidebar:
  order: 3
---

Install `core-vcs`:

```bash
go get github.com/go-steer/core-vcs@latest
```

## 1. Build a `Catalog`

Register the built-in providers (`github`, `gitlab`, `git`) with [`providers.NewRegistry()`](/reference/api-surface/) and configure the forges and repositories your agent is allowed to access:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/go-steer/core-vcs/providers"
	"github.com/go-steer/core-vcs/vcs"
)

func run(ctx context.Context) error {
	cat, err := vcs.NewCatalog(
		providers.NewRegistry(),
		[]vcs.Forge{
			{
				Name:     "github",
				Provider: "github",
				Cred:     vcs.TokenFromEnv("GITHUB_TOKEN"),
				CommitIdentity: vcs.Identity{
					Name:  "gitops-agent",
					Email: "gitops-agent@acme.example",
				},
			},
		},
		[]vcs.RepoConfig{
			{
				Forge:      "github",
				Repository: "github.com/acme/k8s-fleet",
				Role:       vcs.RoleGitOps,
				BaseBranch: "main",
			},
		},
	)
	if err != nil {
		return err
	}

	repo, err := cat.Resolve("acme/k8s-fleet")
	if err != nil {
		return err
	}
```

## 2. Clone, branch, edit, and commit

Use [`vcs.Clone`](/reference/api-surface/) to check out the repository's base branch. Setting [`Policy.CacheDir`](/concepts/shared-cache/) lets concurrent sessions and processes share a single bare object store on disk:

```go
	ws, err := vcs.Clone(ctx, repo, "", "/tmp/session-1/k8s-fleet", vcs.Policy{
		CacheDir: "/var/cache/core-vcs",
	})
	if err != nil {
		return err
	}

	// Create a topic branch under Policy.BranchPrefix (default "agent/").
	if err := ws.Branch("agent/scale-api-replicas"); err != nil {
		return err
	}

	// Modify files in the working tree using standard os calls.
	manifest := filepath.Join(ws.Dir(), "apps/api/deployment.yaml")
	if err := os.WriteFile(manifest, []byte("replicas: 5\n"), 0o644); err != nil {
		return err
	}

	// Inspect diff against the cloned base revision.
	diff, err := ws.Diff(vcs.DiffOptions{AgainstBase: true})
	if err != nil {
		return err
	}
	fmt.Println(diff)

	// Stage and commit all changes.
	cm, err := ws.Commit("feat(api): scale deployment replicas to 5", vcs.Identity{})
	if err != nil {
		return err
	}
	log.Printf("committed %s", cm.Revision)
```

## 3. Publish and open a proposal

[`Workspace.Publish`](/concepts/workspace-and-guards/) verifies all branch and base guards before pushing the topic branch, and [`Repo.CreateProposal`](/reference/api-surface/) opens a proposal on the forge:

```go
	pub, err := ws.Publish(ctx, "")
	if err != nil {
		return err
	}

	prop, err := repo.CreateProposal(ctx, vcs.ProposalRequest{
		Source: pub.Branch,
		Title:  "feat(api): scale deployment replicas to 5",
		Body:   "Increases `apps/api` replicas from 2 to 5 to absorb peak traffic.",
		Draft:  true,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Opened %s #%d: %s\n", "proposal", prop.Number, prop.URL)
	return nil
}
```

## Next steps

- Learn how [`Catalog`, `Forge`, and `Role`](/concepts/architecture/) scope what repositories an agent can read or mutate.
- Read how [`Workspace` publish guards](/concepts/workspace-and-guards/) enforce branch safety in memory.
- Configure [`Policy.CacheDir`](/concepts/shared-cache/) for multi-session and multi-process deployments.
