PLATFORMS := linux/amd64 windows/amd64 darwin/amd64

VERSION=$(shell git describe HEAD --tags --abbrev=0)
GIT_COMMIT=$(shell git rev-parse HEAD)
LD_FLAGS=-ldflags="-X 'github.com/fischor/kubetnl/internal/version.gitCommit=$(GIT_COMMIT)'"
MAIN=./cmd/client/main.go

windows:
	env GOOS=windows GOARCH=amd64 go build ${LD_FLAGS} -o 'bin/kubetnl-$(VERSION)_windows-amd64/kubetnl.exe' ${MAIN}

linux:
	env GOOS=linux GOARCH=amd64 go build ${LD_FLAGS} -o 'bin/kubetnl-$(VERSION)_linux-amd64/kubetnl' ${MAIN}

darwin:
	env GOOS=darwin GOARCH=amd64 go build ${LD_FLAGS} -o 'bin/kubetnl-$(VERSION)_darwin-amd64/kubetnl' ${MAIN}

clean:
	rm -rf bin

version:
	echo ${VERSION}

release: clean windows linux darwin
	zip -r bin/kubetnl-$(VERSION)_windows-amd64.zip bin/kubetnl-$(VERSION)_windows-amd64
	zip -r bin/kubetnl-$(VERSION)_linux-amd64.zip bin/kubetnl-$(VERSION)_linux-amd64
	zip -r bin/kubetnl-$(VERSION)_darwin-amd64.zip bin/kubetnl-$(VERSION)_darwin-amd64
