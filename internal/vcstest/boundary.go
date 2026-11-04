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

package vcstest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// ProviderMarkers maps each provider directory under providers/ (plus
// internal/fakeprovider) to the host strings, API base paths and
// auth-header / clone-user tokens that belong inside that directory and
// nowhere else in the Go tree.
//
// A marker is matched case-insensitively against every non-import string
// literal and every comment in every .go file outside the provider's own
// directory and this file.
var ProviderMarkers = map[string][]string{
	"providers/github": {
		"github.com",
		"api.github.com",
		"www.github.com",
		"ssh.github.com",
		"/api/v3",
		"x-access-token",
		"application/vnd.github",
	},
	"providers/gitlab": {
		"gitlab.com",
		"/api/v4",
		"PRIVATE-TOKEN",
	},
	"internal/fakeprovider": {
		"fake.forge.example",
		"www.fake.forge.example",
		"x-fake-token",
	},
}

// Boundary walks root and returns one violation string per occurrence of
// a provider's markers outside that provider's directory. It is separated
// from CheckBoundary so a test can plant a violation in a scratch tree
// and prove the sweep fails.
func Boundary(root string, markers map[string][]string) ([]string, error) {
	fset := token.NewFileSet()
	var violations []string
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
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || rel == "internal/vcstest/boundary.go" {
			return nil
		}
		fileViolations, err := scanFile(fset, path, rel, markers)
		if err != nil {
			return err
		}
		violations = append(violations, fileViolations...)
		return nil
	})
	return violations, err
}

// CheckBoundary runs Boundary against the module root and fails t on any
// violation. It also asserts that internal/fakeprovider's own markers are
// present inside internal/fakeprovider, so the sweep cannot pass
// vacuously by scanning the wrong root.
func CheckBoundary(t *testing.T) {
	t.Helper()
	root := RepoRoot(t)

	// Non-vacuous check: scanning with no directory exemptions must find
	// fakeprovider's markers inside internal/fakeprovider.
	selfCheck, err := Boundary(root, map[string][]string{
		"nonexistent/dir": ProviderMarkers["internal/fakeprovider"],
	})
	if err != nil {
		t.Fatalf("self-check walk: %v", err)
	}
	if len(selfCheck) == 0 {
		t.Fatalf("boundary sweep found no fakeprovider markers under %s; it would pass vacuously", root)
	}

	violations, err := Boundary(root, ProviderMarkers)
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	for _, v := range violations {
		t.Error(v)
	}
}

func scanFile(fset *token.FileSet, path, rel string, markers map[string][]string) ([]string, error) {
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	imports := map[*ast.BasicLit]bool{}
	for _, imp := range f.Imports {
		imports[imp.Path] = true
	}

	var out []string
	check := func(pos token.Pos, text string) {
		lower := strings.ToLower(text)
		for ownerDir, list := range markers {
			if rel == ownerDir || strings.HasPrefix(rel, ownerDir+"/") {
				continue
			}
			for _, m := range list {
				if strings.Contains(lower, strings.ToLower(m)) {
					p := fset.Position(pos)
					out = append(out, fmt.Sprintf("%s:%d: contains %q (belongs only in %s)", rel, p.Line, m, ownerDir))
				}
			}
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || imports[lit] {
			return true
		}
		val, err := strconv.Unquote(lit.Value)
		if err != nil {
			val = lit.Value
		}
		check(lit.Pos(), val)
		return true
	})
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			check(c.Pos(), c.Text)
		}
	}
	return out, nil
}

// RepoRoot returns the core-vcs module root.
func RepoRoot(tb testing.TB) string {
	tb.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		tb.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		tb.Fatalf("no go.mod at %s: %v", root, err)
	}
	return root
}
