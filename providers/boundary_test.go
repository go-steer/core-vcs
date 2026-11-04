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

package providers_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-steer/core-vcs/internal/vcstest"
	"github.com/go-steer/core-vcs/providers"
)

func TestProviderBoundary(t *testing.T) {
	t.Parallel()
	vcstest.CheckBoundary(t)

	// Prove that a marker outside its owner directory is caught, while the
	// same marker inside its owner directory is allowed.
	scratch := t.TempDir()
	allowed := filepath.Join(scratch, "providers", "sample", "sample.go")
	leaked := filepath.Join(scratch, "vcs", "leak.go")
	if err := os.MkdirAll(filepath.Dir(allowed), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(leaked), 0o750); err != nil {
		t.Fatal(err)
	}
	markers := map[string][]string{"providers/sample": {"api.sample.forge.invalid"}}

	if err := os.WriteFile(allowed, []byte("package sample\nconst base = \"https://api.sample.forge.invalid/v1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := vcstest.Boundary(scratch, markers); err != nil || len(got) != 0 {
		t.Fatalf("allowed marker inside owner dir produced (%v, %v), want no violations", got, err)
	}

	if err := os.WriteFile(leaked, []byte("package vcs\nconst leaked = \"https://api.sample.forge.invalid/v1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := vcstest.Boundary(scratch, markers); err != nil || len(got) != 1 {
		t.Fatalf("leaked marker outside owner dir produced (%v, %v), want 1 violation", got, err)
	}
}

func TestNewRegistry(t *testing.T) {
	t.Parallel()
	if reg := providers.NewRegistry(); reg == nil {
		t.Fatal("providers.NewRegistry() returned nil")
	}
}
