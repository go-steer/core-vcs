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
	"fmt"
	"sort"
	"sync"
)

// Registry maps a provider name to its constructor. It is the one shared
// thing a new forge touches: providers/providers.go holds the table, one
// line per provider. A Registry is safe for concurrent use.
type Registry struct {
	mu           sync.RWMutex
	constructors map[string]Constructor
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{constructors: map[string]Constructor{}}
}

// Register adds a provider. A duplicate or empty name, or a nil
// constructor, is an error.
func (r *Registry) Register(name string, c Constructor) error {
	if name == "" {
		return Errorf(CodeConfigInvalid, "", "provider name is empty")
	}
	if c == nil {
		return Errorf(CodeConfigInvalid, "", "provider %q: nil constructor", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.constructors[name]; dup {
		return Errorf(CodeConfigInvalid, "", "provider %q registered twice", name)
	}
	r.constructors[name] = c
	return nil
}

// MustRegister is Register that panics; for the static table.
func (r *Registry) MustRegister(name string, c Constructor) {
	if err := r.Register(name, c); err != nil {
		panic(err)
	}
}

// Provider constructs the named provider and checks that it answers to
// the name it was registered under. An unknown name is FORGE_UNSUPPORTED.
func (r *Registry) Provider(name string) (Provider, error) {
	r.mu.RLock()
	c, ok := r.constructors[name]
	r.mu.RUnlock()
	if !ok {
		return nil, Errorf(CodeForgeUnsupported, "", "provider %q is not registered; known: %v", name, r.Names())
	}
	p, err := c()
	if err != nil {
		return nil, Errorf(CodeConfigInvalid, "", "provider %q: %w", name, err)
	}
	if p == nil {
		return nil, Errorf(CodeConfigInvalid, "", "provider %q: constructor returned nil", name)
	}
	if p.Name() != name {
		return nil, Errorf(CodeConfigInvalid, "", "provider registered as %q calls itself %q", name, p.Name())
	}
	return p, nil
}

// Names lists the registered providers, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.constructors))
	for n := range r.constructors {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// String lists the registered providers.
func (r *Registry) String() string {
	return fmt.Sprintf("vcs.Registry%v", r.Names())
}
