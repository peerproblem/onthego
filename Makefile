BINARY := dist/onthego
GO ?= go
GOFMT ?= gofmt
VERSION ?= 0.2.0-dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf unknown)
BUILT_AT ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.builtAt=$(BUILT_AT)
PLATFORMS := darwin-arm64 darwin-amd64 linux-arm64 linux-amd64

.PHONY: build test check package clean

build:
	mkdir -p dist
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/onthego

test:
	$(GO) test -count=1 ./...

check:
	test -z "$$($(GOFMT) -l cmd internal)"
	$(GO) vet ./...
	$(GO) test -count=1 ./...

package:
	mkdir -p npm/vendor
	@set -eu; for platform in $(PLATFORMS); do \
		os=$${platform%-*}; arch=$${platform#*-}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o npm/vendor/onthego-$$platform ./cmd/onthego; \
	done
	chmod 0755 npm/bin/onthego.js npm/vendor/*
	npm pack --dry-run

clean:
	rm -rf dist npm/vendor coverage.out
