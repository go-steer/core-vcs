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
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// RepoRef is a repository's identity: a host where the value stated one,
// and an opaque, arbitrary-depth path. It carries no scheme, no
// credential and no port other than one the value spelled out. The path's
// grammar (two segments on one forge, two or more on another) is a
// provider's rule, applied in Provider.Resolve, never here.
type RepoRef struct {
	// Host is lower-case and may carry ":port". Empty when the value named
	// none; a Repo's Ref always has it set.
	Host string
	// Path has no leading or trailing slash and no ".git" suffix. Its case
	// is kept.
	Path string
}

// String renders "host/path", or "path" when Host is empty.
func (r RepoRef) String() string {
	if r.Host == "" {
		return r.Path
	}
	return r.Host + "/" + r.Path
}

// Segments splits Path on "/".
func (r RepoRef) Segments() []string {
	if r.Path == "" {
		return nil
	}
	return strings.Split(r.Path, "/")
}

var (
	scpForm  = regexp.MustCompile(`^[^@/:\s]+@([^@/:\s]+):(.+)$`)
	hostForm = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*(:[0-9]{1,5})?$`)
)

// ParseRepoRef is the one parser. A host is read only where the syntax
// states one: a scheme (https://, http://, ssh://), the SCP form
// user@host:path, or a schemeless value whose first segment is one of
// knownHosts. Any other value is a host-less path, so "my.org/repo" is a
// legal two-segment path on an install that configured no such host.
//
// SCP and ssh:// are accepted as identity syntax only; the clone URL is
// always the provider's HTTPS one. Every segment is checked for traversal
// (".", ".."), a leading "-", whitespace, control characters and the
// characters \ : ? # * [ ~ ^. Failure is REPO_INVALID.
func ParseRepoRef(value string, knownHosts ...string) (RepoRef, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return RepoRef{}, Errorf(CodeRepoInvalid, "", "repository is empty; give owner/name or a URL")
	}
	var host, path string
	switch {
	case strings.Contains(v, "://"):
		u, err := url.Parse(v)
		if err != nil {
			return RepoRef{}, Errorf(CodeRepoInvalid, "", "repository %q: %w", value, err)
		}
		switch u.Scheme {
		case "https", "http":
			if u.User != nil {
				return RepoRef{}, Errorf(CodeRepoInvalid, "", "repository URL must not carry userinfo; pass credentials through Forge.Cred")
			}
		case "ssh":
			if _, hasPassword := u.User.Password(); hasPassword {
				return RepoRef{}, Errorf(CodeRepoInvalid, "", "repository URL must not carry a password; pass credentials through Forge.Cred")
			}
		default:
			return RepoRef{}, Errorf(CodeRepoInvalid, "", "repository %q: scheme %q is not https, http or ssh", value, u.Scheme)
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return RepoRef{}, Errorf(CodeRepoInvalid, "", "repository %q: a query or fragment is not part of a repository", value)
		}
		host, path = u.Host, u.Path
	case scpForm.MatchString(v):
		m := scpForm.FindStringSubmatch(v)
		host, path = m[1], m[2]
	default:
		path = v
		if i := strings.IndexByte(v, '/'); i > 0 {
			first := strings.ToLower(v[:i])
			for _, k := range knownHosts {
				if first == strings.ToLower(k) {
					host, path = v[:i], v[i+1:]
					break
				}
			}
		}
	}
	host = strings.ToLower(host)
	if host != "" && !validHost(host) {
		return RepoRef{}, Errorf(CodeRepoInvalid, "", "repository %q: host %q is not a host name", value, host)
	}
	path, err := cleanPath(value, path)
	if err != nil {
		return RepoRef{}, err
	}
	return RepoRef{Host: host, Path: path}, nil
}

func validHost(host string) bool {
	if !hostForm.MatchString(host) {
		return false
	}
	if _, portStr, hasPort := strings.Cut(host, ":"); hasPort {
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	return true
}

// cleanPath strips the decoration git tolerates and refuses what a
// repository path may not contain.
func cleanPath(value, p string) (string, error) {
	p = strings.Trim(p, "/")
	p = strings.TrimSuffix(p, ".git")
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "", Errorf(CodeRepoInvalid, "", "repository %q: no path after the host", value)
	}
	for _, seg := range strings.Split(p, "/") {
		if err := checkSegment(value, seg); err != nil {
			return "", err
		}
	}
	return p, nil
}

func checkSegment(value, seg string) error {
	switch {
	case seg == "":
		return Errorf(CodeRepoInvalid, "", "repository %q: empty path segment", value)
	case seg == "." || seg == "..":
		return Errorf(CodeRepoInvalid, "", "repository %q: path segment %q is traversal", value, seg)
	case strings.HasPrefix(seg, "-"):
		return Errorf(CodeRepoInvalid, "", "repository %q: path segment %q starts with a dash", value, seg)
	}
	for _, r := range seg {
		if unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(`\:?#*[~^`, r) {
			return Errorf(CodeRepoInvalid, "", "repository %q: path segment %q contains %q", value, seg, r)
		}
	}
	return nil
}
