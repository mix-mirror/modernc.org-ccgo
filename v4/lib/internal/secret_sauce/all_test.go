// Copyright 2025 The qbecc Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sauce

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	rc := m.Run()
	os.Exit(rc)
}

func Test(t *testing.T) {
	t.Logf("TODO")
}
