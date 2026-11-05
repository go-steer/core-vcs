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
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Credential supplies a forge token at call time. It is the seam
// per-caller identity plugs in behind: a provider asks for the token when
// it is about to spend it and never reads an environment variable or a
// file itself.
type Credential interface {
	Token(ctx context.Context) (string, error)
}

// redacted is what every credential prints as, under every verb.
const redacted = "[redacted]"

// CredentialFunc adapts a function to the Credential interface, allowing an
// external credential provider or proxy (resolving per-caller identity from
// ctx or minting a token for a configured subject) to plug into Forge.Cred
// without a wrapper type. It prints as "[redacted]" under every fmt verb.
type CredentialFunc func(ctx context.Context) (string, error)

// Token calls f(ctx) and validates that the returned token is non-empty.
func (f CredentialFunc) Token(ctx context.Context) (string, error) {
	if f == nil {
		return "", Errorf(CodeForgeUnauthorized, "", "credential function is nil")
	}
	tok, err := f(ctx)
	if err != nil {
		var vcsErr *Error
		if errors.As(err, &vcsErr) {
			return "", err
		}
		return "", Errorf(CodeForgeUnauthorized, "", "resolving credential: %w", err)
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", Errorf(CodeForgeUnauthorized, "", "resolved credential is empty")
	}
	return tok, nil
}

func (CredentialFunc) String() string             { return redacted }
func (CredentialFunc) GoString() string           { return redacted }
func (CredentialFunc) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte(redacted)) }

// StaticToken is a credential holding one fixed token. It prints as
// "[redacted]" under %v, %+v, %#v and %s, so a Forge can be logged.
func StaticToken(token string) Credential { return staticToken{token: token} }

type staticToken struct{ token string }

func (s staticToken) Token(context.Context) (string, error) {
	if s.token == "" {
		return "", Errorf(CodeForgeUnauthorized, "", "credential is empty; configure a token")
	}
	return s.token, nil
}

func (staticToken) String() string             { return redacted }
func (staticToken) GoString() string           { return redacted }
func (staticToken) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// TokenFile is a credential read from path on every call, with
// surrounding whitespace trimmed, so a mounted secret that rotates is
// picked up without a restart. The path, not the token, is printable.
func TokenFile(path string) Credential { return tokenFile{path: path} }

type tokenFile struct{ path string }

func (t tokenFile) Token(context.Context) (string, error) {
	b, err := os.ReadFile(t.path)
	if err != nil {
		return "", Errorf(CodeForgeUnauthorized, "", "credential file %s: %w; mount the token", t.path, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", Errorf(CodeForgeUnauthorized, "", "credential file %s is empty; write the token", t.path)
	}
	return tok, nil
}

func (t tokenFile) String() string             { return "TokenFile(" + t.path + ")" }
func (t tokenFile) GoString() string           { return t.String() }
func (t tokenFile) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(t.String())) }

// BasicAuth is what a Workspace presents to the smart-HTTP transport. The
// provider chooses the user; the password is the credential's token.
// Password is withheld from every fmt verb.
type BasicAuth struct {
	Username string
	Password string
}

func (b BasicAuth) String() string   { return "BasicAuth(" + b.Username + ")" }
func (b BasicAuth) GoString() string { return b.String() }
func (b BasicAuth) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(b.String()))
}
