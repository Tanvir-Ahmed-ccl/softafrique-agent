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

## msi: build the unsigned pilot MSI (requires wix on PATH, Windows only)
##
## Both extensions are required: UI.wixext for WixShell and WixQuietExec, and
## Util.wixext for the service configuration. Product.wxs uses both, and a
## missing extension surfaces as a wall of unresolved-element errors rather than
## a useful message.
WIXEXT = -ext WixToolset.UI.wixext -ext WixToolset.Util.wixext

## msi: build the unsigned pilot MSI
msi: build-windows
	cd installer && wix build -arch x64 $(WIXEXT) -o SoftafriqueBackupAgent-$(VERSION)-unsigned.msi

## msi-layout: resolve the WiX source without producing an MSI, for review
msi-layout:
	cd installer && wix build -arch x64 $(WIXEXT) -bindpath ../build/bin/ -o nul

## clean: remove build output
clean:
	rm -rf build/bin installer/build installer/*.wixobj installer/*.wixpdb

## help: list the targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
