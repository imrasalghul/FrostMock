# Build the ChatGPT Bifrost plugin (.so). Go plugins are supported on Linux and
# macOS only — they cannot be built or loaded on Windows. Use the Dockerfile to
# produce a Linux .so from any host.
#
# The Go toolchain used here MUST match the Go version Bifrost itself was built
# with, with matching shared dependencies and build flags on the
# same OS/arch. Mismatches fail to load with "plugin was built with a different
# version of package ...".

PLUGIN_NAME := chatgpt
OUTPUT_DIR  := build
BUILD_FLAGS ?=

UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Linux)
	PLUGIN_EXT := .so
endif
ifeq ($(UNAME_S),Darwin)
	PLUGIN_EXT := .so
endif
PLUGIN_EXT ?= .so

OUTPUT := $(OUTPUT_DIR)/$(PLUGIN_NAME)$(PLUGIN_EXT)

.PHONY: all tidy build login-helper test vet install clean docker

all: build

tidy: ## Resolve and pin dependencies (run after changing go.mod)
	go mod tidy

build: ## Build the plugin for the current platform
	@echo "Building $(OUTPUT)…"
	@mkdir -p $(OUTPUT_DIR)
	go build $(BUILD_FLAGS) -buildmode=plugin -o $(OUTPUT) .
	@echo "Built: $(OUTPUT)"

login-helper: ## Standalone OAuth login helper; no inference server
	@mkdir -p $(OUTPUT_DIR)
	go build $(BUILD_FLAGS) -o $(OUTPUT_DIR)/chatgpt-login .

test:
	go test ./...

vet:
	go vet ./...

install: build ## Build and copy into the Bifrost plugins directory
	@mkdir -p $$HOME/.bifrost/plugins
	@cp $(OUTPUT) $$HOME/.bifrost/plugins/
	@echo "Installed to ~/.bifrost/plugins/$(PLUGIN_NAME)$(PLUGIN_EXT)"

docker: ## Build a Linux .so inside a container (host-independent)
	@mkdir -p $(OUTPUT_DIR)
	docker build -t frostmock .
	docker create --name chatgpt-plugin-tmp frostmock
	docker cp chatgpt-plugin-tmp:/out/$(PLUGIN_NAME).so $(OUTPUT_DIR)/$(PLUGIN_NAME).so
	docker rm chatgpt-plugin-tmp
	@echo "Linux plugin at $(OUTPUT_DIR)/$(PLUGIN_NAME).so"

clean: ## Remove build artifacts
	@rm -rf $(OUTPUT_DIR)
