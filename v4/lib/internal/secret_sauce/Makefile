# Copyright 2025 The qbecc Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

.PHONY:	all clean edit editor test shorttest

all: editor
	golint
	staticcheck

clean:
	rm -f log-* cpu.test mem.test *.out go.work*
	go clean

edit:
	@touch log
	@if [ -f "Session.vim" ]; then gvim -S & else gvim -p Makefile *.go & fi

editor:
	gofmt -l -s -w .
	go test -c -o /dev/null
	staticcheck 2>&1

test:
	go test -v -timeout 24h

shorttest:
	go test -v -timeout 24h -short -failfast
