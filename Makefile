# Softafrique Backup Agent — build, test and packaging helpers.
#
# Development happens on macOS; the customer agent ships as a Windows exe. The
# MSI can only be built on Windows with the WiX toolset on PATH.

VERSION         ?= 0.2.0
WIN_TARGET      ?= build/bin/SoftafriqueBackupAgent.exe
VALIDATE_TARGET ?= build/bin/validatepath.exe
MAC_TARGET      ?= build/bin/agent
RESTIC_VERSION  ?= 0.19.1
LDFLAGS         := -s -w -X main.Version=$(VERSION)

.PHONY: all test race vet build-mac build-windows validatepath dist msi msi-layout clean help

all: vet test build-mac

## test: run the unit tests
test:
	go test ./...

## race: run the tests with the race detector (needs cgo)
race:
	go test -race ./...

## vet: vet for the host and for Windows, which is where the code actually runs
vet:
	go vet ./...
	GOOS=windows GOARCH=amd64 go vet ./...
	GOOS=windows GOARCH=arm64 go build ./...

## build-mac: developer binary for running the tests on a Mac
build-mac:
	@mkdir -p $(dir $(MAC_TARGET))
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(MAC_TARGET) ./cmd/agent

## build-windows: agent.exe plus the bundled restic.exe
build-windows: validatepath
	./scripts/build-windows.sh $(VERSION)

## validatepath: the small native helper the MSI uses to check BACKUPPATH
validatepath:
	@mkdir -p $(dir $(VALIDATE_TARGET))
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
		-ldflags "$(LDFLAGS) -s -w" \
		-o $(VALIDATE_TARGET) ./cmd/validatepath

## dist: everything needed to deploy, verified
dist: build-windows
	@ls -lh build/bin

## wix-preflight: install the pinned WiX toolset and its two extensions
##
## The -ext flags below can only *find* an extension that has already been added
## to the WiX extension folder. Without this step the build fails on a wall of
## unresolved-element errors, which says nothing about the real cause. The
## extensions are pinned to the same version as the tool so they cannot drift
## apart and fail to load.
WIX_VERSION ?= 5.0.2
WIXEXT = -ext WixToolset.UI.wixext -ext WixToolset.Util.wixext

## wix-preflight: install WiX and both .wixext extensions (Windows only)
wix-preflight:
	DOTNET_ROLL_FORWARD=Major dotnet tool install --global wix --version $(WIX_VERSION)
	wix extension add -g WixToolset.UI.wixext/$(WIX_VERSION)
	wix extension add -g WixToolset.Util.wixext/$(WIX_VERSION)

# Product.wxs refers to the payload through two *named* bind paths, and both have
# to be passed here. The paths are absolute on purpose: WiX resolves a relative
# bind path against the .wxs file's own directory rather than the directory the
# command runs in, and getting that wrong does not fail the build -- it quietly
# produces an MSI with an empty Files table. That is why msi asserts a size.
WIXBIND = -bindpath bin=$(CURDIR)/build/bin -bindpath res=$(CURDIR)/installer

## msi: build the unsigned pilot MSI (requires wix on PATH, Windows only)
msi: build-windows
	wix build installer/Product.wxs -arch x64 $(WIXEXT) $(WIXBIND) \
		-o installer/SoftafriqueBackupAgent-$(VERSION)-unsigned.msi
	@size=$$(wc -c < installer/SoftafriqueBackupAgent-$(VERSION)-unsigned.msi); \
		echo "MSI size: $$size bytes"; \
		if [ "$$size" -lt 5000000 ]; then \
			echo "ERROR: the MSI is only $$size bytes, which means the payload was not"; \
			echo "embedded. Check that build/bin holds all three exes and that WIXBIND"; \
			echo "points at it. An empty Files table installs and does nothing."; \
			exit 1; \
		fi

## msi-layout: resolve the WiX source without producing an MSI, for review
msi-layout:
	wix build installer/Product.wxs -arch x64 $(WIXEXT) $(WIXBIND) -o nul

## clean: remove build output
clean:
	rm -rf build/bin installer/build installer/*.wixobj installer/*.wixpdb

## help: list the targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
