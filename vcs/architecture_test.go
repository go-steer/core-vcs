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

package vcs

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	tailLineThreshold           = 150
	maxNonTestFuncsOver150Lines = 3 // vcstest.testVerbs (181), (*gitlab.Provider).ReviewProposal (170), main.startLocalGitServer (160)
)

// packageLayer defines strict acyclic architectural layers across all in-tree
// packages in this module (higher layers may only import strictly lower layers).
var packageLayer = map[string]int{
	// Layer 0: core domain/workspace and standalone git remote server (zero internal imports).
	"vcs":                0,
	"internal/gitremote": 0,

	// Layer 1: individual provider implementations and core test harnesses.
	"internal/fakeprovider": 1,
	"internal/vcstest":      1,
	"providers/git":         1,
	"providers/github":      1,
	"providers/gitlab":      1,

	// Layer 2: provider registry table aggregating individual providers.
	"providers": 2,

	// Layer 3: runnable examples and end-to-end entrypoints.
	"examples/gitops-workflow": 3,
}

func TestArchitectureLayeringAndComplexityTail(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	modulePath, err := readModulePath(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	seenPackages := make(map[string]bool, len(packageLayer))
	var over150 []string

	err = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".worktrees", ".claude", "bin", "dist", "vendor", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, raw, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}

		pkgRel := filepath.ToSlash(filepath.Dir(rel))
		seenPackages[pkgRel] = true
		for _, msg := range checkPackageImports(rel, pkgRel, modulePath, packageLayer, f) {
			t.Error(msg)
		}

		lines := strings.Split(string(raw), "\n")
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			start := fset.Position(fn.Pos()).Line
			end := fset.Position(fn.End()).Line
			if countCodeLines(lines, start, end) > tailLineThreshold {
				over150 = append(over150, rel+":"+formatFuncDeclName(fn))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for pkg := range packageLayer {
		if !seenPackages[pkg] {
			t.Errorf("packageLayer entry %q matched no non-test .go files under repoRoot", pkg)
		}
	}
	if len(over150) > maxNonTestFuncsOver150Lines {
		t.Errorf("non-test functions over %d code lines = %d (ratchet ceiling %d): %v",
			tailLineThreshold, len(over150), maxNonTestFuncsOver150Lines, over150)
	}
}

func TestArchitectureValidators(t *testing.T) {
	t.Parallel()

	const mod = "example.invalid/core"
	layers := map[string]int{
		"vcs":            0,
		"providers/git":  1,
		"providers/peer": 1,
		"providers":      2,
	}

	parseSrc := func(t *testing.T, src string) *ast.File {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), "sample.go", src, parser.ParseComments)
		if err != nil {
			t.Fatalf("ParseFile: %v", err)
		}
		return f
	}

	// 1. Valid downward imports + stdlib + sibling module lookalike pass cleanly.
	validFile := parseSrc(t, `package providers
import (
	"fmt"
	"example.invalid/core-extra/other"
	"example.invalid/core/providers/git"
	"example.invalid/core/vcs"
)
`)
	if errs := checkPackageImports("providers/providers.go", "providers", mod, layers, validFile); len(errs) != 0 {
		t.Fatalf("unexpected errors on valid imports: %v", errs)
	}

	// 2. Unregistered source package is rejected.
	if errs := checkPackageImports("mystery/x.go", "mystery", mod, layers, parseSrc(t, "package mystery\n")); len(errs) != 1 || !strings.Contains(errs[0], "not registered") {
		t.Fatalf("unregistered source errors = %v, want 1 not-registered error", errs)
	}

	// 3. Unregistered internal target, same-layer peer import, upward import
	// (including backtick raw-string import), and root self-import are rejected.
	badFile := parseSrc(t, "package git\nimport (\n"+
		"\t\"example.invalid/core\"\n"+
		"\t`example.invalid/core/providers`\n"+
		"\t\"example.invalid/core/providers/peer\"\n"+
		"\t\"example.invalid/core/unknown\"\n"+
		")\n")
	errs := checkPackageImports("providers/git/git.go", "providers/git", mod, layers, badFile)
	if len(errs) != 4 {
		t.Fatalf("bad imports errors = %v (len %d), want 4", errs, len(errs))
	}

	// 4. countCodeLines ignores blank lines and // line comments and respects start..end bounds.
	sampleLines := []string{
		"package p",
		"",
		"// leading doc",
		"func work() {",
		"	// comment inside body",
		"",
		"	x := 1",
		"	_ = x",
		"}",
		"func trailing() {}",
	}
	if got := countCodeLines(sampleLines, 4, 9); got != 4 {
		t.Fatalf("countCodeLines = %d, want 4", got)
	}

	// 5. formatFuncDeclName formats free functions, value/pointer receivers, and generic receivers.
	declFile := parseSrc(t, `package p
type Plain struct{}
type Box[T any] struct{}
type Pair[K comparable, V any] struct{}
func Free() {}
func (Plain) Val() {}
func (*Plain) Ptr() {}
func (Box[T]) GenVal() {}
func (*Box[T]) GenPtr() {}
func (Pair[K, V]) MultiVal() {}
func (*Pair[K, V]) MultiPtr() {}
`)
	var gotNames []string
	for _, d := range declFile.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok {
			gotNames = append(gotNames, formatFuncDeclName(fn))
		}
	}
	wantNames := []string{
		"Free",
		"(Plain).Val",
		"(*Plain).Ptr",
		"(Box).GenVal",
		"(*Box).GenPtr",
		"(Pair).MultiVal",
		"(*Pair).MultiPtr",
	}
	if strings.Join(gotNames, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("formatFuncDeclName = %v, want %v", gotNames, wantNames)
	}

	// 6. parseModulePath parses unquoted/quoted/raw-quoted module directives and rejects missing directive.
	for _, modSpec := range []string{
		"// comment\n\nmodule example.invalid/core\n\ngo 1.26.10\n",
		"module \"example.invalid/core\"\n",
		"module `example.invalid/core`\n",
	} {
		gotMod, err := parseModulePath(modSpec)
		if err != nil || gotMod != mod {
			t.Fatalf("parseModulePath(%q) = (%q, %v), want (%q, nil)", modSpec, gotMod, err, mod)
		}
	}
	if _, err := parseModulePath("go 1.26.10\n"); err == nil {
		t.Fatal("parseModulePath succeeded without module directive")
	}
}

func checkPackageImports(relFile, pkgRel, modulePath string, layers map[string]int, f *ast.File) []string {
	srcLayer, ok := layers[pkgRel]
	if !ok {
		return []string{fmt.Sprintf("%s: package %q is not registered in packageLayer architecture map", relFile, pkgRel)}
	}
	prefix := modulePath + "/"
	var errs []string
	for _, imp := range f.Imports {
		impPath := unquoteGoString(imp.Path.Value)
		var dstPkg string
		switch {
		case impPath == modulePath:
			dstPkg = "."
		case strings.HasPrefix(impPath, prefix):
			dstPkg = strings.TrimPrefix(impPath, prefix)
		default:
			continue
		}
		dstLayer, known := layers[dstPkg]
		if !known {
			errs = append(errs, fmt.Sprintf("%s: imports unregistered internal package %q", relFile, dstPkg))
			continue
		}
		if dstLayer >= srcLayer {
			errs = append(errs, fmt.Sprintf("layering violation in %s (%s, layer %d): cannot import %s (layer %d)",
				relFile, pkgRel, srcLayer, dstPkg, dstLayer))
		}
	}
	return errs
}

func countCodeLines(lines []string, startLine, endLine int) int {
	n := 0
	for i := startLine - 1; i < endLine && i < len(lines); i++ {
		s := strings.TrimSpace(lines[i])
		if s == "" || strings.HasPrefix(s, "//") {
			continue
		}
		n++
	}
	return n
}

func formatFuncDeclName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recvExpr := fn.Recv.List[0].Type
	ptr := false
	if star, ok := recvExpr.(*ast.StarExpr); ok {
		ptr = true
		recvExpr = star.X
	}
	switch idx := recvExpr.(type) {
	case *ast.IndexExpr:
		recvExpr = idx.X
	case *ast.IndexListExpr:
		recvExpr = idx.X
	}
	if id, ok := recvExpr.(*ast.Ident); ok {
		if ptr {
			return "(*" + id.Name + ")." + fn.Name.Name
		}
		return "(" + id.Name + ")." + fn.Name.Name
	}
	return fn.Name.Name
}

func unquoteGoString(raw string) string {
	if val, err := strconv.Unquote(raw); err == nil {
		return val
	}
	return strings.Trim(raw, "\"`")
}

func readModulePath(gomodPath string) (string, error) {
	raw, err := os.ReadFile(gomodPath)
	if err != nil {
		return "", err
	}
	return parseModulePath(string(raw))
}

func parseModulePath(content string) (string, error) {
	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			mod := unquoteGoString(strings.TrimSpace(rest))
			if mod != "" {
				return mod, nil
			}
		}
	}
	return "", fmt.Errorf("no module directive found in go.mod")
}
