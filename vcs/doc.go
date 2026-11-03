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

// Package vcs is a forge-neutral version-control library for agents that
// run where there is no git binary: a go-git working copy plus provider
// packages that speak each forge's API behind one vocabulary.
//
// A change offered for review is a proposal. A pull request and a merge
// request are the same thing on two forges, and nothing exported here
// uses either spelling.
//
// The design of record is core-agent's docs/gitops-vcs-design.md until
// this module grows its own; see docs/README.md.
package vcs
