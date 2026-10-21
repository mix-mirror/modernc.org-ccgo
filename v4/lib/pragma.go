// Copyright 2026 The CCGO Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccgo // import "modernc.org/ccgo/v4/lib"

import (
	"io/fs"
	"path/filepath"

	"modernc.org/cc/v4"
)

// tuConfig returns the cc configuration for one translation unit. It is a
// copy of t.cfg with its own '#pragma once' handler: the handler keeps the
// files seen so far, so it must not be shared by the units a task compiles in
// parallel.
func (t *Task) tuConfig() *cc.Config {
	cfg := *t.cfg
	cfg.PragmaHandler = pragmaOnce(cfg.FS)
	return &cfg
}

// pragmaOnce returns a cc.Config.PragmaHandler implementing '#pragma once'
// for one translation unit. cc does not implement the pragma itself. The
// handler records the file the first time it sees the pragma in it and, every
// later time, tells cc to skip the rest of the file. Any other pragma is
// ignored.
//
// A file on disk is identified by its canonical path: a header reached
// through two differently spelled include paths, or through a symlink, is
// still one file. A file from fsys is identified by its name.
func pragmaOnce(fsys fs.FS) func([]cc.Token) error {
	seen := map[string]struct{}{}
	return func(toks []cc.Token) error {
		if len(toks) == 0 || toks[0].SrcStr() != "once" {
			return nil
		}

		fn := toks[0].Position().Filename
		if fn == "_Pragma_" {
			// cc scans the operand of the _Pragma operator as a source of this
			// name, so the file using _Pragma("once") is not known. Ignore it.
			return nil
		}

		if fsys == nil {
			if s, err := filepath.Abs(fn); err == nil {
				fn = s
			}
			if s, err := filepath.EvalSymlinks(fn); err == nil {
				fn = s
			}
		}
		if _, ok := seen[fn]; ok {
			return cc.SkipSource
		}

		seen[fn] = struct{}{}
		return nil
	}
}
