.PHONY: build test clean dist-macos dist-macos-unsigned dist-linux dist-windows

VERSION ?= $(shell grep 'var version' main.go | sed 's/.*"\(.*\)"/\1/')
LDFLAGS = -s -w -X main.version=$(VERSION)
BINARY = htmlclay
# macOS needs cgo for the systray and the Apple Event handler; elsewhere the
# build is pure Go.
CGO = 0
ifeq ($(OS),Windows_NT)
	BINARY = htmlclay.exe
endif
ifeq ($(shell uname -s),Darwin)
	CGO = 1
endif

build:
	CGO_ENABLED=$(CGO) go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .
ifeq ($(shell uname -s),Darwin)
	codesign -f -s - $(BINARY)
endif

test:
	CGO_ENABLED=1 go test -race ./... -count=1

clean:
	rm -f htmlclay htmlclay.exe
	rm -rf HTMLClay.app
	rm -f *.dmg

dist-macos:
	bash dist/macos/build.sh

dist-macos-unsigned:
	bash dist/macos/build.sh --unsigned

dist-linux:
	bash dist/linux/build.sh

dist-windows:
	powershell -File dist/windows/build.ps1
