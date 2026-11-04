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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-vcs/internal/vcstest"
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
