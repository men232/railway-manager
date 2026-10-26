.PHONY: build test test-race lint check live live-db shots clean

BINARY  := railway-manager
CMD     := ./cmd/railway-manager
PKG     := github.com/men232/railway-manager/internal/version
VERSION ?= v$(shell cat internal/version/VERSION)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%d)
LDFLAGS := -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).BuildDate=$(DATE)

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

test:
	go test ./...

test-race:
	go test -race ./...

# check is the gofmt + vet part of lint.
check:
	@test -z "$$(gofmt -l .)" || { echo 'Unformatted Go files (run gofmt -w):'; gofmt -l .; exit 1; }
	go vet ./...

lint: check
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; else echo 'golangci-lint not installed: skipped'; fi

# Live tests change a real Railway environment you name with RM_LIVE_PROJECT,
# RM_LIVE_ENV and RM_LIVE_SERVICE (RM_LIVE_WORKER for live-db); it must hold
# the shared variable RM_LIVE_TEST_TARGET=1. See internal/apply/live_test.go.
live:
	LIVE_RAILWAY=1 go test ./internal/apply -run Live -v -timeout 60m

# Changes the Redis password of the live environment, and back.
live-db:
	LIVE_RAILWAY=1 LIVE_DB=1 go test ./internal/apply -run TestLiveDBRotateRedis -v -timeout 60m

# Rerenders the README screenshots from the screen tests' frames (needs Chrome).
shots:
	rm -rf .shots && mkdir -p .shots
	-RM_SHOTS=$(CURDIR)/.shots go test -count=1 ./internal/tui/screens/... >/dev/null
	go run ./tools/shots -in .shots
	rm -rf .shots

clean:
	rm -f $(BINARY)
