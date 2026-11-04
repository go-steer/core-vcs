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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-vcs/vcs"
)

func TestStaticTokenAndRedaction(t *testing.T) {
	t.Parallel()
	const secret = "super-secret-value-9f8e7d"
	cred := vcs.StaticToken(secret)

	tok, err := cred.Token(context.Background())
	if err != nil || tok != secret {
		t.Fatalf("Token() = (%q, %v), want %q", tok, err, secret)
	}

	forge := vcs.Forge{Name: "f", Provider: "p", Cred: cred}
	auth := vcs.BasicAuth{Username: "u", Password: secret}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, v := range []any{cred, forge, auth} {
			rendered := fmt.Sprintf(verb, v)
			if strings.Contains(rendered, secret) {
				t.Errorf("fmt.Sprintf(%q, %T) leaked the token: %s", verb, v, rendered)
			}
		}
	}

	if _, err := vcs.StaticToken("").Token(context.Background()); !vcs.IsCode(err, vcs.CodeForgeUnauthorized) {
		t.Errorf("empty StaticToken error = %v, want CodeForgeUnauthorized", err)
	}
}

func TestTokenFileReReadsOnRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "token")
	cred := vcs.TokenFile(path)

	// Missing file.
	if _, err := cred.Token(ctx); !vcs.IsCode(err, vcs.CodeForgeUnauthorized) {
		t.Fatalf("missing file error = %v, want CodeForgeUnauthorized", err)
	}

	// Empty file.
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cred.Token(ctx); !vcs.IsCode(err, vcs.CodeForgeUnauthorized) {
		t.Fatalf("empty file error = %v, want CodeForgeUnauthorized", err)
	}

	// First rotation.
	if err := os.WriteFile(path, []byte("  first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := cred.Token(ctx); err != nil || got != "first-token" {
		t.Fatalf("first read = (%q, %v), want first-token", got, err)
	}

	// Second rotation is picked up without constructing a new Credential.
	if err := os.WriteFile(path, []byte("second-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := cred.Token(ctx); err != nil || got != "second-token" {
		t.Fatalf("second read = (%q, %v), want second-token", got, err)
	}

	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if rendered := fmt.Sprintf(verb, cred); strings.Contains(rendered, "second-token") {
			t.Errorf("fmt.Sprintf(%q, TokenFile) leaked the token: %s", verb, rendered)
		}
	}
}
