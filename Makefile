APP        := cc-connect
CMD        := ./cmd/cc-connect
DIST       := dist
VERSION    ?= v1.0.2
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
GO         ?= go

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildTime=$(BUILD_TIME)

OPEN_SOURCE_INSTALL_BUNDLE := $(DIST)/cc-connect-source-install

.PHONY: build run clean test test-race verify test-open-source-installer open-source-install-bundle

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(APP) $(CMD)

run: build
	./$(APP)

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

verify:
	$(GO) mod verify
	$(GO) vet ./...
	$(GO) build ./...
	$(GO) test ./...

open-source-install-bundle:
	packaging/macos/package-source-installer.sh "$(OPEN_SOURCE_INSTALL_BUNDLE)"

test-open-source-installer:
	sh tests/open_source_installer/run.sh

clean:
	rm -f $(APP)
	rm -rf $(DIST)
