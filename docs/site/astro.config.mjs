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

// @ts-check
import { defineConfig } from 'astro/config';
import { unified } from '@astrojs/markdown-remark';
import starlight from '@astrojs/starlight';
import starlightLlmsTxt from 'starlight-llms-txt';
import { remarkPrependBase } from './src/plugins/remark-prepend-base.mjs';

const BASE = '/core-vcs';

// Mirrors mast's, core-agent's, and core-models's docs/site (the go-steer
// convention). Project-pages hosting at https://go-steer.github.io/core-vcs/.
// Starlight prefixes only its own chrome with `base`; the
// remark-prepend-base plugin rewrites root-relative content links onto
// it at build time, so authors write `[text](/concepts/...)`.
export default defineConfig({
  site: 'https://go-steer.github.io',
  base: BASE,
  markdown: {
    processor: unified({ remarkPlugins: [remarkPrependBase(BASE)] }),
  },
  integrations: [
    starlight({
      title: 'core-vcs',
      description:
        'Forge-neutral version-control library in pure Go for agents that run where there is no git binary.',
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/go-steer/core-vcs',
        },
      ],
      editLink: {
        baseUrl: 'https://github.com/go-steer/core-vcs/edit/main/docs/site/',
      },
      plugins: [
        starlightLlmsTxt({
          projectName: 'core-vcs',
          description:
            'Forge-neutral version-control library in pure Go for agents that run where there is no git binary: go-git workspaces with in-memory publish guards, shared bare object caches across sessions/processes, and providers for GitHub, GitLab, and bare Git remotes.',
          promote: ['why-core-vcs', 'getting-started'],
          demote: ['reference/**'],
        }),
      ],
      customCss: ['./src/styles/theme.css'],
      sidebar: [
        {
          label: 'Overview',
          items: [
            { label: 'Introduction', link: '/' },
            { label: 'Why core-vcs', link: '/why-core-vcs/' },
            { label: 'Getting started', link: '/getting-started/' },
          ],
        },
        {
          label: 'Concepts',
          items: [{ autogenerate: { directory: 'concepts' } }],
        },
        {
          label: 'Reference',
          items: [{ autogenerate: { directory: 'reference' } }],
        },
      ],
    }),
  ],
});
