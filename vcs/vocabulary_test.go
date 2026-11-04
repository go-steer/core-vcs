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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/go-steer/core-vcs/internal/vcstest"
	"github.com/go-steer/core-vcs/vcs"
)

// forgeReviewSpelling matches any forge-specific noun for a proposal:
// PullRequest, MergeRequest, pull_request, merge-request, or a camelCase /
// snake_case / word-bounded PR / MR token.
var forgeReviewSpelling = regexp.MustCompile(
	`(?i:pull[_-]?request|merge[_-]?request)` +
		`|((^|[_\s-])(PR|MR|pr|mr)s?([_\s-]|$))` +
		`|([a-z0-9](PR|MR)([A-Z]|s\b|$))` +
		`|(^(PR|MR)([A-Z][a-z]|s\b|[0-9]|$))`,
)

func TestNeutralVocabularyEverywhereExported(t *testing.T) {
	t.Parallel()

	// Non-vacuous self-check on the regex.
	for _, bad := range []string{"CreatePR", "PRNumber", "OpenMR", "PullRequest", "merge_request", "ListPRs", "PR_NUMBER", "mr-list"} {
		if !forgeReviewSpelling.MatchString(bad) {
			t.Fatalf("forgeReviewSpelling failed to match banned sample %q", bad)
		}
	}
	for _, ok := range []string{"CreateProposal", "PrimaryRepo", "ParseRepoRef", "ProposalRequest", "PROTECTED_BRANCH", "proposal-create"} {
		if forgeReviewSpelling.MatchString(ok) {
			t.Fatalf("forgeReviewSpelling false-positived on %q", ok)
		}
	}

	root := vcstest.RepoRoot(t)
	fset := token.NewFileSet()
	checked := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || rel == "providers" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || !id.IsExported() {
				return true
			}
			checked++
			if forgeReviewSpelling.MatchString(id.Name) {
				p := fset.Position(id.Pos())
				t.Errorf("%s:%d: exported identifier %q uses forge-specific review vocabulary; use Proposal", rel, p.Line, id.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if checked == 0 {
		t.Fatal("checked 0 exported identifiers; walk is broken")
	}

	for _, c := range vcs.Codes {
		if forgeReviewSpelling.MatchString(string(c)) {
			t.Errorf("Code %q uses forge-specific vocabulary", c)
		}
	}
	for _, v := range vcs.ProposalVerbs {
		if forgeReviewSpelling.MatchString(string(v)) {
			t.Errorf("Verb %q uses forge-specific vocabulary", v)
		}
	}
}

func TestProviderAbstractsOnlyTheRemoteHalf(t *testing.T) {
	t.Parallel()
	localOnly := map[string]bool{
		"Status": true, "Diff": true, "Log": true, "Commit": true,
		"Annotate": true, "Blame": true, "Files": true, "Grep": true,
	}
	pt := reflect.TypeFor[vcs.Provider]()
	for i := 0; i < pt.NumMethod(); i++ {
		m := pt.Method(i).Name
		if localOnly[m] {
			t.Errorf("vcs.Provider has local-only method %q; local git verbs belong on Workspace", m)
		}
	}
}
