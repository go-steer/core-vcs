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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-vcs/internal/fakeprovider"
	"github.com/go-steer/core-vcs/internal/vcstest"
	"github.com/go-steer/core-vcs/vcs"
)

func TestTLSConfigAndTransportError(t *testing.T) {
	t.Parallel()
	fx := fakeprovider.NewTLSFixture(t)

	// Without CAFile, dialing the self-signed fixture fails TLS verification
	// and TransportError classifies it as FORGE_TLS_UNTRUSTED naming CAFile.
	untrustedForge := vcs.Forge{Name: "corp", Provider: fakeprovider.Name, Host: "corp.example"}
	client, err := vcs.NewHTTPClient(untrustedForge)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	_, rawErr := client.Get(fx.Server.URL)
	classified := vcs.TransportError(untrustedForge, vcs.VerbProposalList, rawErr)
	vcstest.RequireError(t, classified, vcs.CodeForgeTLSUntrusted, "corp")
	if !strings.Contains(classified.Detail, "CAFile") {
		t.Errorf("FORGE_TLS_UNTRUSTED Detail %q does not name CAFile", classified.Detail)
	}

	// With CAFile set to the fixture's PEM, the same call succeeds.
	trustedForge := untrustedForge
	trustedForge.CAFile = fx.CAFile
	trustedClient, err := vcs.NewHTTPClient(trustedForge)
	if err != nil {
		t.Fatalf("NewHTTPClient(CAFile): %v", err)
	}
	resp, err := trustedClient.Get(fx.Server.URL)
	if err != nil {
		t.Fatalf("GET with CAFile failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401 from fixture", resp.StatusCode)
	}

	// Missing CAFile and non-PEM CAFile are both FORGE_TLS_UNTRUSTED.
	_, err = vcs.TLSConfig(vcs.Forge{Name: "corp", CAFile: filepath.Join(t.TempDir(), "missing.pem")})
	vcstest.RequireError(t, err, vcs.CodeForgeTLSUntrusted, "corp")

	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = vcs.TLSConfig(vcs.Forge{Name: "corp", CAFile: garbage})
	vcstest.RequireError(t, err, vcs.CodeForgeTLSUntrusted, "corp")

	// Context deadline / cancellation -> FORGE_UNAVAILABLE (retryable).
	te := vcs.TransportError(untrustedForge, vcs.VerbProposalView, context.DeadlineExceeded)
	vcstest.RequireError(t, te, vcs.CodeForgeUnavailable, "corp")
	if !te.Code.Retryable() {
		t.Error("FORGE_UNAVAILABLE.Retryable() = false, want true")
	}
	for _, c := range vcs.Codes {
		if c != vcs.CodeForgeUnavailable && c.Retryable() {
			t.Errorf("%s.Retryable() = true, want only FORGE_UNAVAILABLE retryable", c)
		}
	}
}
