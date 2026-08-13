.PHONY: build test clean sync-conformance sync-selector-parity differential dist-macos dist-macos-unsigned dist-linux dist-windows

# Where the conformance corpus is generated. Only needed by sync-conformance; the vendored copy
# under dataapi/testdata is checked in so `go test` never needs node or a sibling checkout.
HYPER_HTML_API ?= ../hyper-html-api

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

# Vendors the parity corpus from hyper-html-api. Run it after every `npm run conformance:generate`
# over there, and commit the result alongside whatever moved the contract. A stale copy does not
# fail loudly — it just tests the old contract — so VERSION records what was copied and
# TestConformanceCorpusIsCurrent compares it against the manifest.
sync-conformance:
	@test -d "$(HYPER_HTML_API)/conformance" || \
		{ echo "no conformance dir at $(HYPER_HTML_API); set HYPER_HTML_API=<path to hyper-html-api>"; exit 1; }
	rm -rf dataapi/testdata/conformance
	mkdir -p dataapi/testdata
	cp -R "$(HYPER_HTML_API)/conformance" dataapi/testdata/conformance
	@printf 'Copied from %s by `make sync-conformance`. Do not edit here; edit the source and re-sync.\n\n' \
		"hyper-html-api/conformance" > dataapi/testdata/conformance/VERSION
	@cat dataapi/testdata/conformance/MANIFEST.json >> dataapi/testdata/conformance/VERSION

# Re-records what cheerio answers for every selector construct the gate has an opinion about. The
# gate is the one part of dataapi with no JS counterpart, so this baseline is its only real proof;
# regenerate it whenever the allow-list or the reference's cheerio version changes.
sync-selector-parity:
	@test -d "$(HYPER_HTML_API)/node_modules/cheerio" || \
		{ echo "no cheerio at $(HYPER_HTML_API); set HYPER_HTML_API=<path to hyper-html-api>"; exit 1; }
	HYPER_HTML_API="$(HYPER_HTML_API)" node scripts/gen-selector-parity.mjs dataapi/testdata/selector-parity.json
	@echo "synced $$(ls dataapi/testdata/conformance/cases/*.meta | wc -l | tr -d ' ') cases"

# Cross-language differential: random (document, rules) pairs through cheerio and then through the Go
# port. A DEVELOPMENT harness, never a CI gate — htmlclay vendors no JS engine, so a test that skipped
# when node was missing would read as parity coverage while checking nothing. Run it after any change
# to dataapi, and persist whatever it finds as a conformance case.
differential:
	@test -d "$(HYPER_HTML_API)/node_modules/cheerio" || \
		{ echo "no cheerio at $(HYPER_HTML_API); set HYPER_HTML_API=<path to hyper-html-api>"; exit 1; }
	HYPER_HTML_API="$(HYPER_HTML_API)" node scripts/differential.mjs $(ARGS)

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
