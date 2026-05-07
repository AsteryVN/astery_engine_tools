# Astery Engine Tools — Makefile
# Targets: build, test, run, clean, tauri-dev, tauri-build

GO         ?= go
GOFLAGS    ?= -trimpath
LDFLAGS    ?= -s -w
BIN_DIR    ?= bin
DAEMON     ?= engine-toold
VERSION    ?= 0.1.0-dev

.PHONY: build test run clean lint tauri-dev tauri-build

build:
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS) -X main.version=$(VERSION)" -o $(BIN_DIR)/$(DAEMON) ./cmd/$(DAEMON)
	@echo "built $(BIN_DIR)/$(DAEMON) ($(VERSION))"

test:
	$(GO) test ./... -race -count=1

lint:
	$(GO) vet ./...

run: build
	./$(BIN_DIR)/$(DAEMON) --headless

clean:
	rm -rf $(BIN_DIR) data/

tauri-dev:
	cd tauri-app && cargo tauri dev

tauri-build:
	cd tauri-app && cargo tauri build
