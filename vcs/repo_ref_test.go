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
	"reflect"
	"testing"

	"github.com/go-steer/core-vcs/vcs"
)

func TestParseRepoRefAccept(t *testing.T) {
	t.Parallel()
	known := []string{"git.corp.example", "code.internal:8443"}
	cases := []struct {
		in   string
		want vcs.RepoRef
		segs []string
	}{
		{
			in:   "acme/infra",
			want: vcs.RepoRef{Path: "acme/infra"},
			segs: []string{"acme", "infra"},
		},
		{
			// A dotted first segment that is NOT a configured forge host
			// stays a path segment: "my.org/repo" is a legal bare slug.
			in:   "my.org/Platform-Repo.git",
			want: vcs.RepoRef{Path: "my.org/Platform-Repo"},
			segs: []string{"my.org", "Platform-Repo"},
		},
		{
			// The same shape with a known forge host splits host from path.
			in:   "GIT.CORP.EXAMPLE/platform/clusters/prod/",
			want: vcs.RepoRef{Host: "git.corp.example", Path: "platform/clusters/prod"},
			segs: []string{"platform", "clusters", "prod"},
		},
		{
			in:   "code.internal:8443/acme/infra.git",
			want: vcs.RepoRef{Host: "code.internal:8443", Path: "acme/infra"},
			segs: []string{"acme", "infra"},
		},
		{
			in:   "https://Git.Corp.Example/acme/infra.git",
			want: vcs.RepoRef{Host: "git.corp.example", Path: "acme/infra"},
			segs: []string{"acme", "infra"},
		},
		{
			in:   "http://code.internal:8443/a/b/c/d",
			want: vcs.RepoRef{Host: "code.internal:8443", Path: "a/b/c/d"},
			segs: []string{"a", "b", "c", "d"},
		},
		{
			in:   "ssh://git@git.corp.example/acme/infra.git",
			want: vcs.RepoRef{Host: "git.corp.example", Path: "acme/infra"},
			segs: []string{"acme", "infra"},
		},
		{
			in:   "git@git.corp.example:acme/infra.git",
			want: vcs.RepoRef{Host: "git.corp.example", Path: "acme/infra"},
			segs: []string{"acme", "infra"},
		},
	}
	for _, tc := range cases {
		got, err := vcs.ParseRepoRef(tc.in, known...)
		if err != nil {
			t.Errorf("ParseRepoRef(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseRepoRef(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		if !reflect.DeepEqual(got.Segments(), tc.segs) {
			t.Errorf("ParseRepoRef(%q).Segments() = %v, want %v", tc.in, got.Segments(), tc.segs)
		}
	}
}

func TestParseRepoRefReject(t *testing.T) {
	t.Parallel()
	bad := []string{
		"",
		"   ",
		"/",
		".git",
		"https://git.corp.example/",
		"https://git.corp.example/.git",
		"git://git.corp.example/acme/infra",
		"file:///tmp/repo",
		"https://git.corp.example/acme/infra?ref=main",
		"https://git.corp.example/acme/infra#readme",
		"https://token@git.corp.example/acme/infra",
		"http://user:pass@git.corp.example/acme/infra",
		"ssh://git:pass@git.corp.example/acme/infra",
		"https://git.corp.example:0/acme/infra",
		"https://git.corp.example:70000/acme/infra",
		"https://bad_host/acme/infra",
		"acme/../*",
		"../acme/infra",
		"acme/../infra",
		"acme/./infra",
		"acme//infra",
		"-acme/infra",
		"acme/-infra",
		"acme/in fra",
		"acme/in\tfra",
		"acme/in\nfra",
		`acme/in\fra`,
		"acme/in:fra",
		"acme/in?fra",
		"acme/in#fra",
		"acme/in~fra",
		"acme/in^fra",
	}
	for _, in := range bad {
		got, err := vcs.ParseRepoRef(in, "git.corp.example")
		if !vcs.IsCode(err, vcs.CodeRepoInvalid) {
			t.Errorf("ParseRepoRef(%q) = (%+v, %v), want CodeRepoInvalid", in, got, err)
		}
	}
}
