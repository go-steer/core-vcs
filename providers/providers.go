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

// Package providers builds the default vcs.Registry. It is the one shared
// file a new forge touches: one import and one r.MustRegister line. A
// provider does not enter this table until it passes the contract test.
package providers

import (
	"github.com/go-steer/core-vcs/providers/git"
	"github.com/go-steer/core-vcs/providers/github"
	"github.com/go-steer/core-vcs/vcs"
)

// NewRegistry returns a Registry populated with every shipped provider.
func NewRegistry() *vcs.Registry {
	r := vcs.NewRegistry()
	r.MustRegister(git.Name, git.New)
	r.MustRegister(github.Name, github.New)
	return r
}
