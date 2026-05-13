## chaos-harness Makefile
##
## Targets:
##   build              builds all four binaries with the system Go toolchain
##   build-go125        builds chaos-worker with Go 1.25 (run 7 only)
##   matrix             runs the full seven-run matrix
##   matrix-quick       60s smoke run of the matrix
##   run RUN=<id>       single run by ID (expects test-matrix/<id>.yaml)
##   aggregate RUN=<id> re-aggregate without re-running
##   test               go test ./...
##   vet                go vet ./...
##   fmt                gofmt -s -w
##   clean              remove build artifacts and results

GO          ?= go
GO125       ?= go1.25
BIN_DIR     := bin
RESULTS_DIR := results
LDFLAGS     := -s -w -X main.version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

BINARIES := chaos-worker chaos-launcher chaos-victim chaos-observer chaos-tuned

.PHONY: all build build-go125 matrix matrix-quick run aggregate test vet fmt clean help

all: build

help:
	@grep -E '^##' Makefile | sed 's/^## //'

build: $(addprefix $(BIN_DIR)/, $(BINARIES))

$(BIN_DIR)/%:
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags='$(LDFLAGS)' -o $@ ./cmd/$*

build-go125:
	@mkdir -p $(BIN_DIR)
	$(GO125) build -ldflags='$(LDFLAGS)' -o $(BIN_DIR)/chaos-worker-go125 ./cmd/chaos-worker

matrix: build
	./scripts/run-matrix.sh

matrix-quick: build
	DURATION=60s ./scripts/run-matrix.sh

run: build
	@if [ -z "$(RUN)" ]; then echo "usage: make run RUN=<id>"; exit 2; fi
	./$(BIN_DIR)/chaos-launcher --config test-matrix/$(RUN).yaml --output-dir $(RESULTS_DIR)/

aggregate:
	@if [ -z "$(RUN)" ]; then echo "usage: make aggregate RUN=<id>"; exit 2; fi
	python3 scripts/aggregate-results.py --run-dir $(RESULTS_DIR)/$(RUN)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -s -w .

## clean              remove build artifacts (bin/ only)
## clean-results      remove per-run output subdirs under results/

clean:
	rm -rf $(BIN_DIR)

clean-results:
	@find $(RESULTS_DIR) -mindepth 1 -maxdepth 1 -type d -exec rm -rf {} +
