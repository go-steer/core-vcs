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
	"bufio"
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-steer/core-vcs/internal/vcstest"
	"github.com/go-steer/core-vcs/vcs"
)

// TestEveryImportablePackageIsADecision enforces that every importable
// library package in the module (outside dev/, examples/ and internal/) is
// explicitly classified as either `promised` or `unsupported` in
// dev/api-surface.txt, and that every entry in dev/api-surface.txt
// corresponds to a real package directory.
func TestEveryImportablePackageIsADecision(t *testing.T) {
	t.Parallel()
	root := vcstest.RepoRoot(t)
	surfaceFile := filepath.Join(root, "dev", "api-surface.txt")

	f, err := os.Open(surfaceFile)
	if err != nil {
		t.Fatalf("open %s: %v", surfaceFile, err)
	}
	defer func() { _ = f.Close() }()

	decisions := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || (fields[0] != "promised" && fields[0] != "unsupported") {
			t.Errorf("dev/api-surface.txt:%d: want '<promised|unsupported> <pkg>', got %q", lineNo, line)
			continue
		}
		kind, pkg := fields[0], fields[1]
		if prev, dup := decisions[pkg]; dup {
			t.Errorf("dev/api-surface.txt:%d: duplicate entry for %s (already %s)", lineNo, pkg, prev)
			continue
		}
		decisions[pkg] = kind
		if info, statErr := os.Stat(filepath.Join(root, pkg)); statErr != nil || !info.IsDir() {
			t.Errorf("dev/api-surface.txt:%d: listed package directory %q does not exist", lineNo, pkg)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", surfaceFile, err)
	}

	cmd := exec.Command("go", "list", "-f", "{{.Dir}}", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list ./...: %v\n%s", err, out)
	}

	importable := make(map[string]bool)
	for _, dir := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dir = strings.TrimSpace(dir)
		if dir == "" || dir == root {
			continue
		}
		rel, relErr := filepath.Rel(root, dir)
		if relErr != nil {
			t.Fatalf("filepath.Rel(%q, %q): %v", root, dir, relErr)
		}
		rel = filepath.ToSlash(rel)
		if isExemptFromAPISurface(rel) {
			continue
		}
		importable[rel] = true
		if _, ok := decisions[rel]; !ok {
			t.Errorf("importable package %q is not classified in dev/api-surface.txt (add `promised %s` or `unsupported %s`)", rel, rel, rel)
		}
	}
	for pkg := range decisions {
		if !importable[pkg] {
			t.Errorf("dev/api-surface.txt entry %q is not a non-exempt importable Go package in `go list ./...`", pkg)
		}
	}
}

func isExemptFromAPISurface(rel string) bool {
	return rel == "internal" || strings.HasPrefix(rel, "internal/") ||
		strings.Contains(rel, "/internal/") || strings.HasSuffix(rel, "/internal") ||
		rel == "dev" || strings.HasPrefix(rel, "dev/") ||
		rel == "examples" || strings.HasPrefix(rel, "examples/")
}

// TestEveryPresubmitHasACIStep enforces that every script in
// dev/ci/presubmits/ has a corresponding entry in dev/tools/ci's STEPS array.
func TestEveryPresubmitHasACIStep(t *testing.T) {
	t.Parallel()
	root := vcstest.RepoRoot(t)

	ciPath := filepath.Join(root, "dev", "tools", "ci")
	ciBytes, err := os.ReadFile(ciPath)
	if err != nil {
		t.Fatalf("read %s: %v", ciPath, err)
	}
	ciText := string(ciBytes)

	presubmitDir := filepath.Join(root, "dev", "ci", "presubmits")
	entries, err := os.ReadDir(presubmitDir)
	if err != nil {
		t.Fatalf("readdir %s: %v", presubmitDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		target := "dev/tools/" + name
		if _, statErr := os.Stat(filepath.Join(root, target)); statErr != nil {
			t.Errorf("presubmit %s has no corresponding tool script %s: %v", name, target, statErr)
		}
		if !strings.Contains(ciText, target) {
			t.Errorf("presubmit dev/ci/presubmits/%s is not invoked in dev/tools/ci (expected %q in STEPS)", name, target)
		}
	}
}

var (
	reVCSQualIdent  = regexp.MustCompile(`\bvcs\.([A-Z][A-Za-z0-9_]*)\b`)
	reTypeMember    = regexp.MustCompile(`\b([A-Z][A-Za-z0-9_]*)\.([A-Z][A-Za-z0-9_]*)\b`)
	reBacktickIdent = regexp.MustCompile("`([A-Z][A-Za-z0-9_]*(?:Config|Code)|(?:Code|Token)[A-Z][A-Za-z0-9_]*)`")
	reBacktickCall  = regexp.MustCompile("`([A-Z][A-Za-z0-9_]*)\\(")
	reBacktickCode  = regexp.MustCompile("`([A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)`")
)

type vcsSurface struct {
	exported        map[string]bool
	typeNames       map[string]bool
	methodNames     map[string]bool
	members         map[string]bool
	funcSigs        map[string]string
	methodSigs      map[string]string
	structFields    map[string]string
	ifaceMethods    map[string]string
	underlyingTypes map[string]string
	constValues     map[string]string
}

// TestDocsSiteReferencesRealExportedSurface verifies that every exported
// package vcs symbol, struct field, method, error code, and API reference
// declaration mentioned across docs/site/src/content/docs/ corresponds to a
// real exported declaration in package vcs.
func TestDocsSiteReferencesRealExportedSurface(t *testing.T) {
	t.Parallel()
	root := vcstest.RepoRoot(t)
	surf := parseVCSExportedSurface(t, filepath.Join(root, "vcs"))

	docsRoot := filepath.Join(root, "docs", "site", "src", "content", "docs")
	seenErrorRefCodes := make(map[vcs.Code]bool)

	err := filepath.WalkDir(docsRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || (!strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".mdx")) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(raw), "\n")
		inGoFence := false
		isAPISurfaceRef := strings.HasSuffix(rel, "/reference/api-surface.md")
		isErrorCodesRef := strings.HasSuffix(rel, "/reference/error-codes.md")
		var apiSurfaceGo strings.Builder

		for idx, line := range lines {
			lineNo := idx + 1
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "```") {
				if !inGoFence {
					inGoFence = strings.HasPrefix(trimmed, "```go")
				} else {
					inGoFence = false
					if isAPISurfaceRef {
						apiSurfaceGo.WriteByte('\n')
					}
				}
				continue
			}

			if isAPISurfaceRef && inGoFence {
				apiSurfaceGo.WriteString(line)
				apiSurfaceGo.WriteByte('\n')
			}

			for _, m := range reVCSQualIdent.FindAllStringSubmatch(line, -1) {
				sym := m[1]
				if !surf.exported[sym] {
					t.Errorf("%s:%d: docs reference non-existent symbol vcs.%s", rel, lineNo, sym)
				}
			}

			for _, m := range reTypeMember.FindAllStringSubmatch(line, -1) {
				lhs, rhs := m[1], m[2]
				if !surf.typeNames[lhs] {
					continue
				}
				if !surf.members[lhs+"."+rhs] {
					t.Errorf("%s:%d: docs reference non-existent field or method %s.%s", rel, lineNo, lhs, rhs)
				}
			}

			if !inGoFence {
				for _, m := range reBacktickIdent.FindAllStringSubmatch(line, -1) {
					sym := m[1]
					if !surf.exported[sym] && !surf.methodNames[sym] {
						t.Errorf("%s:%d: docs reference non-existent symbol %q", rel, lineNo, sym)
					}
				}
				for _, m := range reBacktickCall.FindAllStringSubmatch(line, -1) {
					sym := m[1]
					if sym == "NewRegistry" {
						continue // providers.NewRegistry
					}
					if sym == "Open" && (strings.Contains(line, "No `Open(") || strings.Contains(line, "no `Open(")) {
						continue // explicit negative invariant ("no Open(dir) constructor")
					}
					if !surf.exported[sym] && !surf.methodNames[sym] {
						t.Errorf("%s:%d: docs reference non-existent function or method %s(...)", rel, lineNo, sym)
					}
				}
				for _, m := range reBacktickCode.FindAllStringSubmatch(line, -1) {
					code := vcs.Code(m[1])
					if !code.Valid() {
						t.Errorf("%s:%d: docs reference unknown error code %q", rel, lineNo, m[1])
						continue
					}
					if isErrorCodesRef {
						seenErrorRefCodes[code] = true
					}
				}
			}
		}

		if isAPISurfaceRef {
			verifyAPISurfaceDecls(t, rel, apiSurfaceGo.String(), surf)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", docsRoot, err)
	}

	for _, c := range vcs.Codes {
		if !seenErrorRefCodes[c] {
			t.Errorf("docs/site/src/content/docs/reference/error-codes.md is missing vcs.Code %q", c)
		}
	}
}

func verifyAPISurfaceDecls(t *testing.T, rel, snippet string, surf vcsSurface) {
	t.Helper()
	src := "package vcs\n\nimport (\n\t\"context\"\n\t\"time\"\n)\n\n" + snippet
	fset := token.NewFileSet()
	fileAST, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		t.Fatalf("parse Go fences in %s: %v", rel, err)
	}

	for _, decl := range fileAST.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sig := canonicalFuncSig(fset, d.Type)
			if d.Recv == nil {
				want, ok := surf.funcSigs[d.Name.Name]
				if !ok {
					t.Errorf("%s: api-surface declares non-existent function %s", rel, d.Name.Name)
					continue
				}
				if sig != want {
					t.Errorf("%s: function %s signature = %s, want %s", rel, d.Name.Name, sig, want)
				}
				continue
			}
			recv := baseIdent(d.Recv.List[0].Type)
			key := recv + "." + d.Name.Name
			want, ok := surf.methodSigs[key]
			if !ok {
				t.Errorf("%s: api-surface declares non-existent method %s", rel, key)
				continue
			}
			if sig != want {
				t.Errorf("%s: method %s signature = %s, want %s", rel, key, sig, want)
			}

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					for i, id := range s.Names {
						if !surf.exported[id.Name] {
							t.Errorf("%s: api-surface declares non-existent const/var %s", rel, id.Name)
							continue
						}
						if i < len(s.Values) {
							gotVal := exprString(fset, s.Values[i])
							if wantVal, ok := surf.constValues[id.Name]; ok && gotVal != wantVal {
								t.Errorf("%s: const %s value = %s, want %s", rel, id.Name, gotVal, wantVal)
							}
						}
					}

				case *ast.TypeSpec:
					tName := s.Name.Name
					if !surf.typeNames[tName] {
						t.Errorf("%s: api-surface declares non-existent type %s", rel, tName)
						continue
					}
					switch td := s.Type.(type) {
					case *ast.StructType:
						for _, field := range td.Fields.List {
							gotType := exprString(fset, field.Type)
							for _, id := range field.Names {
								key := tName + "." + id.Name
								wantType, ok := surf.structFields[key]
								if !ok {
									t.Errorf("%s: api-surface declares non-existent struct field %s", rel, key)
									continue
								}
								if gotType != wantType {
									t.Errorf("%s: struct field %s type = %s, want %s", rel, key, gotType, wantType)
								}
							}
						}
					case *ast.InterfaceType:
						for _, m := range td.Methods.List {
							ft, ok := m.Type.(*ast.FuncType)
							if !ok || len(m.Names) == 0 {
								continue
							}
							key := tName + "." + m.Names[0].Name
							gotSig := canonicalFuncSig(fset, ft)
							wantSig, exists := surf.ifaceMethods[key]
							if !exists {
								t.Errorf("%s: api-surface declares non-existent interface method %s", rel, key)
								continue
							}
							if gotSig != wantSig {
								t.Errorf("%s: interface method %s signature = %s, want %s", rel, key, gotSig, wantSig)
							}
						}
					default:
						gotUnder := canonicalUnderlying(fset, s.Type)
						if wantUnder, ok := surf.underlyingTypes[tName]; ok && gotUnder != wantUnder {
							t.Errorf("%s: type %s underlying = %s, want %s", rel, tName, gotUnder, wantUnder)
						}
					}
				}
			}
		}
	}
}

// TestGettingStartedSnippetCompiles stitches the Go blocks in
// docs/site/src/content/docs/getting-started.md and compiles them against the
// local module so copy-paste drift fails unit tests immediately.
func TestGettingStartedSnippetCompiles(t *testing.T) {
	t.Parallel()
	root := vcstest.RepoRoot(t)
	docPath := filepath.Join(root, "docs", "site", "src", "content", "docs", "getting-started.md")
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}

	var snippet strings.Builder
	inGoFence := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inGoFence {
				inGoFence = strings.HasPrefix(trimmed, "```go")
			} else {
				inGoFence = false
				snippet.WriteByte('\n')
			}
			continue
		}
		if inGoFence {
			snippet.WriteString(line)
			snippet.WriteByte('\n')
		}
	}
	if snippet.Len() == 0 {
		t.Fatalf("%s contains no ```go code fences", docPath)
	}
	snippet.WriteString("\nfunc main() { _ = run }\n")

	tmp := t.TempDir()
	modPath, goFloor, toolchain := readModuleDirective(t, filepath.Join(root, "go.mod"))
	goSum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatalf("read go.sum: %v", err)
	}
	var goMod strings.Builder
	goMod.WriteString("module example.com/getting-started-check\n\ngo " + goFloor + "\n")
	if toolchain != "" {
		goMod.WriteString("\ntoolchain " + toolchain + "\n")
	}
	goMod.WriteString("\nrequire " + modPath + " v0.0.0\n\nreplace " + modPath + " => " + root + "\n")
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte(goMod.String()), 0o600); err != nil {
		t.Fatalf("write temp go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "go.sum"), goSum, 0o600); err != nil {
		t.Fatalf("write temp go.sum: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "main.go"), []byte(snippet.String()), 0o600); err != nil {
		t.Fatalf("write temp main.go: %v", err)
	}

	cmd := exec.Command("go", "build", "-mod=mod", "-o", filepath.Join(tmp, "getting-started"), ".")
	cmd.Dir = tmp
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("getting-started.md Go snippet failed to compile: %v\n%s", err, out)
	}
}

func parseVCSExportedSurface(t *testing.T, vcsDir string) vcsSurface {
	t.Helper()
	entries, err := os.ReadDir(vcsDir)
	if err != nil {
		t.Fatalf("readdir %s: %v", vcsDir, err)
	}

	fset := token.NewFileSet()
	surf := vcsSurface{
		exported:        make(map[string]bool),
		typeNames:       make(map[string]bool),
		methodNames:     make(map[string]bool),
		members:         make(map[string]bool),
		funcSigs:        make(map[string]string),
		methodSigs:      make(map[string]string),
		structFields:    make(map[string]string),
		ifaceMethods:    make(map[string]string),
		underlyingTypes: make(map[string]string),
		constValues:     make(map[string]string),
	}
	embedded := make(map[string][]string)

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fileAST, parseErr := parser.ParseFile(fset, filepath.Join(vcsDir, name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		for _, decl := range fileAST.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					if ast.IsExported(d.Name.Name) {
						surf.exported[d.Name.Name] = true
						surf.funcSigs[d.Name.Name] = canonicalFuncSig(fset, d.Type)
					}
					continue
				}
				if !ast.IsExported(d.Name.Name) || len(d.Recv.List) == 0 {
					continue
				}
				if recvType := baseIdent(d.Recv.List[0].Type); ast.IsExported(recvType) {
					key := recvType + "." + d.Name.Name
					surf.members[key] = true
					surf.methodNames[d.Name.Name] = true
					surf.methodSigs[key] = canonicalFuncSig(fset, d.Type)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						for i, id := range s.Names {
							if ast.IsExported(id.Name) {
								surf.exported[id.Name] = true
								if i < len(s.Values) {
									surf.constValues[id.Name] = exprString(fset, s.Values[i])
								}
							}
						}
					case *ast.TypeSpec:
						tName := s.Name.Name
						if !ast.IsExported(tName) {
							continue
						}
						surf.exported[tName] = true
						surf.typeNames[tName] = true
						switch td := s.Type.(type) {
						case *ast.StructType:
							for _, field := range td.Fields.List {
								if len(field.Names) == 0 {
									if emb := baseIdent(field.Type); ast.IsExported(emb) {
										embedded[tName] = append(embedded[tName], emb)
									}
									continue
								}
								ft := exprString(fset, field.Type)
								for _, id := range field.Names {
									if ast.IsExported(id.Name) {
										key := tName + "." + id.Name
										surf.members[key] = true
										surf.structFields[key] = ft
									}
								}
							}
						case *ast.InterfaceType:
							for _, m := range td.Methods.List {
								if len(m.Names) == 0 {
									if emb := baseIdent(m.Type); ast.IsExported(emb) {
										embedded[tName] = append(embedded[tName], emb)
									}
									continue
								}
								ft, ok := m.Type.(*ast.FuncType)
								for _, id := range m.Names {
									if ast.IsExported(id.Name) {
										key := tName + "." + id.Name
										surf.members[key] = true
										surf.methodNames[id.Name] = true
										if ok {
											surf.ifaceMethods[key] = canonicalFuncSig(fset, ft)
										}
									}
								}
							}
						default:
							surf.underlyingTypes[tName] = canonicalUnderlying(fset, s.Type)
						}
					}
				}
			}
		}
	}

	for parent, children := range embedded {
		for _, child := range children {
			prefix := child + "."
			for key := range surf.members {
				if strings.HasPrefix(key, prefix) {
					member := strings.TrimPrefix(key, prefix)
					surf.members[parent+"."+member] = true
					if ft, ok := surf.structFields[key]; ok {
						surf.structFields[parent+"."+member] = ft
					}
					if ms, ok := surf.methodSigs[key]; ok {
						surf.methodSigs[parent+"."+member] = ms
					}
					if is, ok := surf.ifaceMethods[key]; ok {
						surf.ifaceMethods[parent+"."+member] = is
					}
				}
			}
		}
	}

	return surf
}

func canonicalUnderlying(fset *token.FileSet, expr ast.Expr) string {
	if ft, ok := expr.(*ast.FuncType); ok {
		return canonicalFuncSig(fset, ft)
	}
	return exprString(fset, expr)
}

func canonicalFuncSig(fset *token.FileSet, ft *ast.FuncType) string {
	params := expandFieldTypes(fset, ft.Params)
	results := expandFieldTypes(fset, ft.Results)
	return "(" + strings.Join(params, ", ") + ") -> (" + strings.Join(results, ", ") + ")"
}

func expandFieldTypes(fset *token.FileSet, fl *ast.FieldList) []string {
	if fl == nil {
		return nil
	}
	var out []string
	for _, field := range fl.List {
		ts := exprString(fset, field.Type)
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		for range count {
			out = append(out, ts)
		}
	}
	return out
}

func exprString(fset *token.FileSet, expr ast.Expr) string {
	var buf bytes.Buffer
	_ = printer.Fprint(&buf, fset, expr)
	return buf.String()
}

func readModuleDirective(t *testing.T, goModPath string) (string, string, string) {
	t.Helper()
	raw, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read %s: %v", goModPath, err)
	}
	var modPath, goFloor, toolchain string
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[0] == "module" {
			modPath = fields[1]
		}
		if len(fields) == 2 && fields[0] == "go" {
			goFloor = fields[1]
		}
		if len(fields) == 2 && fields[0] == "toolchain" {
			toolchain = fields[1]
		}
	}
	if modPath == "" || goFloor == "" {
		t.Fatalf("%s missing module (%q) or go (%q) directive", goModPath, modPath, goFloor)
	}
	return modPath, goFloor, toolchain
}

func baseIdent(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return baseIdent(e.X)
	default:
		return ""
	}
}
