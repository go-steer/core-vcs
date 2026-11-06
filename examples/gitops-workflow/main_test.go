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

package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunWorkflow(t *testing.T) {
	t.Parallel()
	out, err := runWorkflow(context.Background())
	if err != nil {
		t.Fatalf("runWorkflow: %v", err)
	}
	for _, want := range []string{
		"branch:   agent/scale-replicas",
		"commit:   feat(deploy): scale replicas from 2 to 5",
		"/acme/k8s-fleet/+/compare/main...agent/scale-replicas",
		"+replicas: 5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
