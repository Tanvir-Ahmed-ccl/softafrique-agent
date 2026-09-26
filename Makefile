# Softafrique Backup Agent — build & dev helpers.
# Development happens on macOS; the customer agent ships as a Windows exe.

VERSION        ?= 0.1.0
WIN_TARGET     ?= build/bin/SoftafriqueBackupAgent.exe
MAC_TARGET     ?= build/bin/agent
RESTIC_VERSION ?= 0.19.1

.PHONY: all test vet build-mac build-windows dist clean

all: vet test build-mac

test:
	go test ./...

vet:
	go vet ./...
	GOOS=windows GOARCH=amd64 go vet ./cmd/agent ./internal/service

build-mac:
	mkdir -p build/bin
	go build -trimpath -ldflags "-s -w -X main.Version=$(VERSION)" -o $(MAC_TARGET) ./cmd/agent

build-windows:
	./scripts/build-windows.sh $(VERSION)

# build-windows also fetches restic.exe (see script); dist just re-verifies.
dist: build-windows
	ls -lh build/bin

clean:
	rm -rf build/bin