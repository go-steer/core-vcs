---
title: Shared object cache & worktrees
description: Use Policy.CacheDir so concurrent sessions and multiple agent processes share packfiles on disk.
sidebar:
  order: 3
---

When an agent runtime serves multiple concurrent sessions — or multiple agent pods/processes mounted on a shared persistent volume — cloning the same repository from scratch for every session wastes network bandwidth and duplicates `.git/objects/pack` on disk.

Setting `Policy.CacheDir` enables `core-vcs`'s **shared bare object cache and linked worktrees**:

```go
ws, err := vcs.Clone(ctx, repo, "", sessionWorkDir, vcs.Policy{
	CacheDir: "/mnt/shared-vcs-cache",
})
```

## How it works

```
/mnt/shared-vcs-cache/
└── github.com/acme/infra.git          ◄── shared bare object store (packfiles)
└── github.com/acme/infra.git.lock     ◄── advisory flock held ONLY during Fetch

/tmp/session-1/infra/                  ◄── Session 1 working tree
└── .git/
    ├── HEAD, index, config, refs/     ◄── private to Session 1
    └── objects/
        ├── info/alternates            ◄── points to shared bare objects/
        └── <new session commits>      ◄── written only here

/tmp/session-2/infra/                  ◄── Session 2 working tree (concurrent)
└── .git/
    ├── HEAD, index, config, refs/     ◄── private to Session 2
    └── objects/
        ├── info/alternates
        └── <new session commits>
```

### 1. Incremental bare cache sync under `flock`

For a repository `host/path`, `Clone` resolves `<CacheDir>/<host>/<path>.git` and acquires an OS-level advisory lock (`flock` via `go-billy`, retrying on `EINTR`) on `<CacheDir>/<host>/<path>.git.lock`.

While holding the lock, `Clone` initializes the bare repository if needed and runs an incremental single-branch `Fetch` (`+refs/heads/<branch>:refs/heads/<branch>`). Only missing objects since the last fetch are downloaded from the forge. As soon as `Fetch` resolves the target commit hash, the lock is released.

### 2. Isolated session `.git/` + read-through `overlayStorage`

Each session directory gets its own independent `.git/` directory containing its own `HEAD`, `index`, `config`, `refs/`, and local `objects/` directory:

- **Writes are 100% session-private**: Creating a branch (`ws.Branch`), staging files, committing (`ws.Commit`), or fetching a scratch ref during `BASE_MOVED` checks writes only to the session's private `.git/`. Sessions never mutate the shared bare cache after `Clone` returns.
- **Reads are layered (`overlayStorage`)**: `core-vcs` wires a layered `storage.Storer` that reads historical commits, trees, blobs, and delta base objects from the shared bare cache's packfile index in memory, falling back to the session's private `.git/objects` for objects created during the session.
- **Standard Git compatibility (`.git/objects/info/alternates`)**: `Clone` also writes `.git/objects/info/alternates` pointing at `<CacheDir>/<host>/<path>.git/objects`, so any external tool inspecting the worktree sees the complete object graph.

### 3. Safe across goroutines and OS processes

Because the shared bare cache is only mutated under `flock` by additive packfile fetches during `Clone` and opened with `ExclusiveAccess: false` by session worktrees, multiple goroutines in one daemon and multiple OS processes sharing a persistent disk can `Clone`, `Branch`, `Commit`, `Diff`, `Log`, and `Publish` concurrently without lock contention or race conditions.
