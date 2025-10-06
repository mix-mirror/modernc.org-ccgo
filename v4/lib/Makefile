# Copyright 2022 The CCGO Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

.PHONY:	build_all_targets clean edit editor tmp x csmith fftest shorttest sqlitetest debug install work

build_all_targets:
	GOOS=darwin GOARCH=amd64 go build -v ./...
	GOOS=darwin GOARCH=amd64 go test -o /dev/null -c
	GOOS=darwin GOARCH=arm64 go build -v ./...
	GOOS=darwin GOARCH=arm64 go test -o /dev/null -c
	GOOS=freebsd GOARCH=386 go build -v ./...
	GOOS=freebsd GOARCH=386 go test -o /dev/null -c
	GOOS=freebsd GOARCH=amd64 go build -v ./...
	GOOS=freebsd GOARCH=arm go test -o /dev/null -c
	GOOS=freebsd GOARCH=arm64 go build -v ./...
	GOOS=linux GOARCH=386 go build -v ./...
	GOOS=linux GOARCH=386 go test -o /dev/null -c
	GOOS=linux GOARCH=amd64 go build -v ./...
	GOOS=linux GOARCH=amd64 go test -o /dev/null -c
	GOOS=linux GOARCH=arm go build -v ./...
	GOOS=linux GOARCH=arm go test -o /dev/null -c
	GOOS=linux GOARCH=arm64 go build -v ./...
	GOOS=linux GOARCH=arm64 go test -o /dev/null -c
	GOOS=linux GOARCH=ppc64le go build -v ./...
	GOOS=linux GOARCH=ppc64le go test -o /dev/null -c
	GOOS=linux GOARCH=riscv64 go build -v ./...
	GOOS=linux GOARCH=riscv64 go test -o /dev/null -c
	GOOS=linux GOARCH=s390x go build -v ./...
	GOOS=linux GOARCH=s390x go test -o /dev/null -c
	GOOS=netbsd GOARCH=amd64 go build -v ./...
	GOOS=netbsd GOARCH=arm go test -o /dev/null -c
	GOOS=openbsd GOARCH=amd64 go build -v ./...
	GOOS=openbsd GOARCH=amd64 go test -o /dev/null -c
	GOOS=openbsd GOARCH=arm64 go build -v ./...
	GOOS=openbsd GOARCH=arm64 go test -o /dev/null -c
	GOOS=windows GOARCH=386 go build -v ./...
	GOOS=windows GOARCH=386 go test -o /dev/null -c
	GOOS=windows GOARCH=amd64 go build -v ./...
	GOOS=windows GOARCH=amd64 go test -o /dev/null -c
	GOOS=windows GOARCH=arm64 go build -v ./...
	GOOS=windows GOARCH=arm64 go test -o /dev/null -c

clean:
	rm -f log-* cpu.test mem.test *.out
	go clean

edit:
	@if [ -f "Session.vim" ]; then gvim -S & else gvim -p Makefile ../go.mod builder.json all_test.go ccgo.go & fi

edit2:
	@if [ -f "Session.vim" ]; then novim -S & else novim -p Makefile ../go.mod builder.json all_test.go ccgo.go & fi

editor:
	stringer -output stringer.go -type mode,name
	gofmt -l -s -w *.go
	go test -o /dev/null -c
	go install -v -tags=ccgo.assert,ccgo.dmesg modernc.org/ccgo/v4
	staticcheck

test shorttest:
	go test -v -timeout 36h -short -failfast -tags=ccgo.assert -trc -shelltimeout 1m
	git status testdata
	git diff testdata/

work:
	rm -f go.work*
	go work init
	go work use ..
	go work use $(GOPATH)/src/modernc.org/cc/v4
	go work use $(GOPATH)/src/modernc.org/gc/v2
