# core-vcs docs site

The user-facing documentation site for `core-vcs`, built with
[Astro](https://astro.build) and [Starlight](https://starlight.astro.build).
It uses the same setup as `mast`, `core-agent`, and `core-models`.

The design surface is [`docs/api-design.md`](../api-design.md) and
core-agent's [`docs/gitops-vcs-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/gitops-vcs-design.md).

## Run it locally

Use the dev-tools script. It checks your Node version, installs
dependencies when needed, and runs what CI runs:

```sh
dev/tools/docs-site.sh          # dev server, reachable via --host
dev/tools/docs-site.sh build    # astro build
dev/tools/docs-site.sh preview  # serve the built site
dev/tools/docs-site.sh check    # build + link verification (what CI runs)
```

Requires Node 22+ (Astro 7 needs >= 22.12); `check` also needs `python3`.

## Links and the deploy base

The site deploys under `/core-vcs` (project pages at
`https://go-steer.github.io/core-vcs/`).

- **Markdown links:** write them root-relative and **without** the base,
  for example `[text](/concepts/architecture/)`. The `remark-prepend-base`
  plugin adds the base at build time.
- **Everything else:** component props (`LinkCard href`), frontmatter
  (`hero.actions`, `banner`) and raw HTML need the `/core-vcs` prefix
  written out.

`scripts/verify-internal-links.py` fails the build on a dead link or a
missing prefix.

## Deploying

`.github/workflows/docs.yml` deploys on pushes to `main` that touch the
site. Pages has to be enabled once per repository: Settings → Pages →
Source → "GitHub Actions".
