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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"strings"
)

// TLSConfig builds the TLS configuration for f: system roots, plus
// f.CAFile when set. It is the one place CAFile is read, so every
// provider and the Workspace agree on what a self-managed host trusts
// and FORGE_TLS_UNTRUSTED can name the file.
func TLSConfig(f Forge) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if f.CAFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(f.CAFile)
	if err != nil {
		return nil, Errorf(CodeForgeTLSUntrusted, f.Name, "CAFile %s: %w; mount the CA bundle", f.CAFile, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, Errorf(CodeForgeTLSUntrusted, f.Name, "CAFile %s holds no PEM certificate; check the bundle", f.CAFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// NewHTTPClient returns an *http.Client whose transport trusts what
// TLSConfig(f) trusts. Deadlines come from the caller's context.
func NewHTTPClient(f Forge) (*http.Client, error) {
	tlsCfg, err := TLSConfig(f)
	if err != nil {
		return nil, err
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	t := base.Clone()
	t.TLSClientConfig = tlsCfg
	return &http.Client{Transport: t}, nil
}

// TransportError classifies an error from an HTTP round trip or a git
// transport that never produced a response: a certificate that did not
// verify is FORGE_TLS_UNTRUSTED and the Detail names CAFile; a context
// deadline or cancellation is FORGE_UNAVAILABLE; anything else is
// FORGE_CALL_FAILED. An err that is already an *Error is returned as is.
func TransportError(f Forge, verb Verb, err error) *Error {
	if err == nil {
		return nil
	}
	var already *Error
	if errors.As(err, &already) {
		return already
	}
	if isTLSError(err) {
		hint := "the certificate chain is not trusted"
		if f.CAFile == "" {
			hint += "; set CAFile to the host's CA bundle"
		} else {
			hint += "; CAFile " + f.CAFile + " does not cover it"
		}
		return Errorf(CodeForgeTLSUntrusted, f.Name, "%s: %w", hint, err).WithVerb(verb)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return Errorf(CodeForgeUnavailable, f.Name, "call did not complete: %w; retry", err).WithVerb(verb)
	}
	return Errorf(CodeForgeCallFailed, f.Name, "%w", err).WithVerb(verb)
}

func isTLSError(err error) bool {
	var (
		unknownAuthority x509.UnknownAuthorityError
		hostname         x509.HostnameError
		invalid          x509.CertificateInvalidError
		recordHeader     tls.RecordHeaderError
		certVerify       *tls.CertificateVerificationError
	)
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) ||
		errors.As(err, &invalid) || errors.As(err, &recordHeader) || errors.As(err, &certVerify) {
		return true
	}
	// Some transports flatten the x509 error into text before it reaches us.
	msg := err.Error()
	return strings.Contains(msg, "x509:") || strings.Contains(msg, "tls:")
}
