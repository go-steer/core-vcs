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
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/diff"
	dmp "github.com/sergi/go-diff/diffmatchpatch"
)

// Diff returns a unified diff of the working tree against HEAD (when
// opts.AgainstBase is false) or against ClonedRevision (when
// opts.AgainstBase is true). Both committed and uncommitted changes are
// included when diffing against the cloned base. No network.
func (w *Workspace) Diff(opts DiffOptions) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	var baseHash plumbing.Hash
	if opts.AgainstBase {
		baseHash = plumbing.NewHash(w.clonedRevision)
	} else {
		head, err := w.git.Head()
		if err != nil {
			return "", Errorf(CodeGitFailed, w.repo.Forge.Name, "reading HEAD: %w", err)
		}
		baseHash = head.Hash()
	}

	commitObj, err := w.git.CommitObject(baseHash)
	if err != nil {
		return "", Errorf(CodeGitFailed, w.repo.Forge.Name, "loading commit %s: %w", baseHash, err)
	}
	tree, err := commitObj.Tree()
	if err != nil {
		return "", Errorf(CodeGitFailed, w.repo.Forge.Name, "loading tree for %s: %w", baseHash, err)
	}

	fromFiles, err := readTreeSnapshot(tree)
	if err != nil {
		return "", Errorf(CodeGitFailed, w.repo.Forge.Name, "reading base tree: %w", err)
	}
	toFiles, err := readWorktreeSnapshot(w.dir)
	if err != nil {
		return "", Errorf(CodeGitFailed, w.repo.Forge.Name, "reading working tree: %w", err)
	}

	pathSet := map[string]bool{}
	for p := range fromFiles {
		pathSet[p] = true
	}
	for p := range toFiles {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		if len(opts.Paths) > 0 && !slices.Contains(opts.Paths, p) {
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var filePatches []fdiff.FilePatch
	for _, p := range paths {
		from, hasFrom := fromFiles[p]
		to, hasTo := toFiles[p]
		if hasFrom && hasTo && from.hash == to.hash && from.mode == to.mode {
			continue
		}
		filePatches = append(filePatches, buildFilePatch(from, hasFrom, to, hasTo))
	}
	if len(filePatches) == 0 {
		return "", nil
	}

	var buf bytes.Buffer
	enc := fdiff.NewUnifiedEncoder(&buf, fdiff.DefaultContextLines)
	if err := enc.Encode(unifiedPatch{files: filePatches}); err != nil {
		return "", Errorf(CodeGitFailed, w.repo.Forge.Name, "encoding diff: %w", err)
	}
	return buf.String(), nil
}

type fileSnap struct {
	path   string
	mode   filemode.FileMode
	hash   plumbing.Hash
	data   string
	binary bool
}

func (f fileSnap) Hash() plumbing.Hash     { return f.hash }
func (f fileSnap) Mode() filemode.FileMode { return f.mode }
func (f fileSnap) Path() string            { return f.path }

func readTreeSnapshot(tree *object.Tree) (map[string]fileSnap, error) {
	out := map[string]fileSnap{}
	err := tree.Files().ForEach(func(f *object.File) error {
		bin, err := f.IsBinary()
		if err != nil {
			return err
		}
		var content string
		if !bin {
			content, err = f.Contents()
			if err != nil {
				return err
			}
		}
		out[f.Name] = fileSnap{
			path:   f.Name,
			mode:   f.Mode,
			hash:   f.Hash,
			data:   content,
			binary: bin,
		}
		return nil
	})
	return out, err
}

func readWorktreeSnapshot(root string) (map[string]fileSnap, error) {
	scoped, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = scoped.Close() }()

	out := map[string]fileSnap{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		raw, err := scoped.ReadFile(rel)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := filemode.Regular
		if info.Mode()&0o111 != 0 {
			mode = filemode.Executable
		}
		bin := isBinaryBytes(raw)
		var text string
		if !bin {
			text = string(raw)
		}
		relSlash := filepath.ToSlash(rel)
		out[relSlash] = fileSnap{
			path:   relSlash,
			mode:   mode,
			hash:   plumbing.ComputeHash(plumbing.BlobObject, raw),
			data:   text,
			binary: bin,
		}
		return nil
	})
	return out, err
}

func isBinaryBytes(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return true
	}
	return !utf8.Valid(b)
}

func buildFilePatch(from fileSnap, hasFrom bool, to fileSnap, hasTo bool) fdiff.FilePatch {
	var fromFile, toFile fdiff.File
	if hasFrom {
		fromFile = from
	}
	if hasTo {
		toFile = to
	}
	if (hasFrom && from.binary) || (hasTo && to.binary) {
		return unifiedFilePatch{from: fromFile, to: toFile, binary: true}
	}
	var chunks []fdiff.Chunk
	for _, d := range diff.Do(from.data, to.data) {
		var op fdiff.Operation
		switch d.Type {
		case dmp.DiffEqual:
			op = fdiff.Equal
		case dmp.DiffDelete:
			op = fdiff.Delete
		case dmp.DiffInsert:
			op = fdiff.Add
		}
		chunks = append(chunks, unifiedChunk{content: d.Text, op: op})
	}
	return unifiedFilePatch{from: fromFile, to: toFile, chunks: chunks}
}

type unifiedPatch struct{ files []fdiff.FilePatch }

func (p unifiedPatch) FilePatches() []fdiff.FilePatch { return p.files }
func (p unifiedPatch) Message() string                { return "" }

type unifiedFilePatch struct {
	from, to fdiff.File
	chunks   []fdiff.Chunk
	binary   bool
}

func (fp unifiedFilePatch) IsBinary() bool                  { return fp.binary }
func (fp unifiedFilePatch) Files() (fdiff.File, fdiff.File) { return fp.from, fp.to }
func (fp unifiedFilePatch) Chunks() []fdiff.Chunk           { return fp.chunks }

type unifiedChunk struct {
	content string
	op      fdiff.Operation
}

func (c unifiedChunk) Content() string       { return strings.ReplaceAll(c.content, "\r\n", "\n") }
func (c unifiedChunk) Type() fdiff.Operation { return c.op }
