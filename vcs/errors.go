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
	"errors"
	"fmt"
	"strings"
)

// Code names why a verb was refused or failed. The set is closed: every
// error this module returns carries one, and a consumer switches on it
// rather than on a message or an HTTP status. The comment on each code is
// the next action a caller should take.
type Code string

const (
	// Identity and configuration.

	// CodeConfigInvalid is a forge or catalog entry the core refused; fix
	// the configuration.
	CodeConfigInvalid Code = "CONFIG_INVALID"
	// CodeRepoInvalid is a repository value ParseRepoRef or a provider's
	// grammar refused; fix the value.
	CodeRepoInvalid Code = "REPO_INVALID"
	// CodeRepoUnknown is a repository on a configured forge that the
	// catalog does not list; add it with a role.
	CodeRepoUnknown Code = "REPO_UNKNOWN"
	// CodeForgeUnsupported is a host no forge serves, a verb the forge does
	// not offer, or a provider the registry does not know; there is no
	// fallback.
	CodeForgeUnsupported Code = "FORGE_UNSUPPORTED"
	// CodeRoleVCSVerbs is a mutating verb on a RoleContext repository; the
	// repository is read-only by declared intent.
	CodeRoleVCSVerbs Code = "ROLE_VCS_VERBS"

	// Workspace guards. The guards themselves live on Workspace; their
	// codes are fixed here so every consumer sees one set.

	// CodeTargetIsBranch is a publish of the base branch itself; pick a
	// branch under the prefix.
	CodeTargetIsBranch Code = "TARGET_IS_BRANCH"
	// CodeBranchNotOurs is a branch this process did not create or that
	// lacks the branch prefix; clone again and branch from the base.
	CodeBranchNotOurs Code = "BRANCH_NOT_OURS"
	// CodeBaseMoved is a base rewritten under the target since clone;
	// clone again.
	CodeBaseMoved Code = "BASE_MOVED"
	// CodeBranchMoved is a remote tip that is not our last published tip;
	// someone else wrote the branch, clone again.
	CodeBranchMoved Code = "BRANCH_MOVED"
	// CodeBranchDiverged is an existing remote branch with history this
	// copy does not hold; pick another name.
	CodeBranchDiverged Code = "BRANCH_DIVERGED"
	// CodeProtectedBranch is a branch the forge protects against the bot.
	CodeProtectedBranch Code = "PROTECTED_BRANCH"
	// CodeBaseBranchMissing is a configured base the remote does not hold.
	CodeBaseBranchMissing Code = "BASE_BRANCH_MISSING"
	// CodeCloneTooLarge is a working tree over the ceiling named in Detail.
	CodeCloneTooLarge Code = "CLONE_TOO_LARGE"
	// CodeNothingToCommit is an empty tree or no change since HEAD.
	CodeNothingToCommit Code = "NOTHING_TO_COMMIT"
	// CodeGitFailed is a go-git failure the workspace could not classify.
	CodeGitFailed Code = "GIT_FAILED"

	// Forge calls.

	// CodeForgeUnauthorized is a credential the forge refused (401, or 403
	// without rate-limit headers); stop, no retry helps.
	CodeForgeUnauthorized Code = "FORGE_UNAUTHORIZED"
	// CodeForgeUnavailable is a 5xx or a throttle; retry the same call
	// later. The only retryable code.
	CodeForgeUnavailable Code = "FORGE_UNAVAILABLE"
	// CodeForgeRejected is a request the forge refused on its content
	// (422, a refused push); fix a field, do not repeat the call.
	CodeForgeRejected Code = "FORGE_REJECTED"
	// CodeForgeCallFailed is anything else, with the forge's own message
	// verbatim in Detail.
	CodeForgeCallFailed Code = "FORGE_CALL_FAILED"
	// CodeForgeTLSUntrusted is a certificate that did not verify; Detail
	// names CAFile.
	CodeForgeTLSUntrusted Code = "FORGE_TLS_UNTRUSTED"
	// CodeForgeIdentityUnknown is a credential that cannot name its login.
	CodeForgeIdentityUnknown Code = "FORGE_IDENTITY_UNKNOWN"
	// CodeLabelsUnsupported is a label field on a forge without labels.
	CodeLabelsUnsupported Code = "LABELS_UNSUPPORTED"
)

// Codes is every code in the set, for tests and documentation.
var Codes = []Code{
	CodeConfigInvalid, CodeRepoInvalid, CodeRepoUnknown, CodeForgeUnsupported, CodeRoleVCSVerbs,
	CodeTargetIsBranch, CodeBranchNotOurs, CodeBaseMoved, CodeBranchMoved, CodeBranchDiverged,
	CodeProtectedBranch, CodeBaseBranchMissing, CodeCloneTooLarge, CodeNothingToCommit, CodeGitFailed,
	CodeForgeUnauthorized, CodeForgeUnavailable, CodeForgeRejected, CodeForgeCallFailed,
	CodeForgeTLSUntrusted, CodeForgeIdentityUnknown, CodeLabelsUnsupported,
}

// Retryable reports whether repeating the same call unchanged can succeed.
func (c Code) Retryable() bool { return c == CodeForgeUnavailable }

// Valid reports whether c is in the set.
func (c Code) Valid() bool {
	for _, k := range Codes {
		if k == c {
			return true
		}
	}
	return false
}

// Error is the one error type this module returns. Detail says the cause
// and then the action that follows from it; where a forge produced the
// refusal its own message rides in Detail verbatim.
type Error struct {
	Code   Code
	Forge  string // Forge.Name; "" before a forge is chosen
	Verb   Verb   // "" where no verb applies
	Detail string
	cause  error
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Forge != "" || e.Verb != "" {
		b.WriteString(" [")
		if e.Forge != "" {
			b.WriteString("forge ")
			b.WriteString(e.Forge)
		}
		if e.Verb != "" {
			if e.Forge != "" {
				b.WriteString(", ")
			}
			b.WriteString(string(e.Verb))
		}
		b.WriteString("]")
	}
	return b.String()
}

// Unwrap returns the cause given to Errorf through %w, if any.
func (e *Error) Unwrap() error { return e.cause }

// WithVerb returns a copy of e that names v. A nil receiver stays nil.
func (e *Error) WithVerb(v Verb) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Verb = v
	return &c
}

// Errorf builds an Error. format and args follow fmt.Errorf, including %w
// to wrap a cause; the formatted text becomes Detail.
func Errorf(code Code, forge string, format string, args ...any) *Error {
	wrapped := fmt.Errorf(format, args...)
	return &Error{Code: code, Forge: forge, Detail: wrapped.Error(), cause: errors.Unwrap(wrapped)}
}

// CodeOf returns the Code carried by err, or "" when err is not an *Error.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e.Code
	}
	return ""
}

// IsCode reports whether err carries c.
func IsCode(err error, c Code) bool { return CodeOf(err) == c }
