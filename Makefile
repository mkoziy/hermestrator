APP_DIR := app
PM_ENV_FILE ?= $(APP_DIR)/.env
GENKIT_REFLECTION_PORT ?= 3100
GENKIT_CLI_VERSION := 1.15.5
GENKIT_BIN ?= $(APP_DIR)/.bin/genkit
GENKIT_OS := $(shell uname -s | tr '[:upper:]' '[:lower:]')
GENKIT_ARCH := $(shell uname -m | sed -e 's/^arm64$$/arm64/' -e 's/^aarch64$$/arm64/' -e 's/^x86_64$$/x64/' -e 's/^amd64$$/x64/')
GENKIT_CLI_URL := https://storage.googleapis.com/genkit-assets-cli/prod/$(GENKIT_OS)-$(GENKIT_ARCH)/v$(GENKIT_CLI_VERSION)/genkit

.PHONY: check fmt-check mod-check vet lint test race shell-check install-hooks pm-run genkit-cli pm-dev

check: fmt-check mod-check vet lint test race shell-check

fmt-check:
	@test -z "$$(gofmt -l $$(find $(APP_DIR) -name '*.go' -type f))"

mod-check:
	@cd $(APP_DIR) && go mod tidy -diff

vet:
	@cd $(APP_DIR) && go vet ./...

lint:
	@cd $(APP_DIR) && GOLANGCI_LINT_CACHE=$${GOLANGCI_LINT_CACHE:-/private/tmp/hermestrator-golangci-cache} go tool golangci-lint run

test:
	@cd $(APP_DIR) && go test ./...

race:
	@cd $(APP_DIR) && go test -race ./internal/dashboard ./internal/live

shell-check:
	@sh -n $(APP_DIR)/scripts/*.sh

install-hooks:
	@git config core.hooksPath .githooks

# pm-run loads local, ignored development settings only for the dashboard
# process. Production must provide its environment through its runtime.
pm-run:
	@test -f "$(PM_ENV_FILE)" || { echo "missing environment file: $(PM_ENV_FILE)"; exit 1; }
	@set -a; . "$(PM_ENV_FILE)"; set +a; cd "$(APP_DIR)" && exec go run ./cmd/pm

# genkit-cli installs the exact Developer UI CLI version used by the project
# image into an ignored project-local directory. It never changes host PATH.
genkit-cli:
	@if [ -x "$(GENKIT_BIN)" ] && "$(GENKIT_BIN)" --version 2>/dev/null | grep -q "$(GENKIT_CLI_VERSION)"; then exit 0; fi; \
	mkdir -p "$(dir $(GENKIT_BIN))"; \
	curl --fail --location --silent --show-error "$(GENKIT_CLI_URL)" -o "$(GENKIT_BIN).tmp"; \
	chmod 755 "$(GENKIT_BIN).tmp"; \
	mv "$(GENKIT_BIN).tmp" "$(GENKIT_BIN)"; \
	"$(GENKIT_BIN)" --version

# pm-dev runs the Genkit Developer UI locally. It is diagnostic-only and must
# never be exposed as the authenticated operator dashboard.
pm-dev: genkit-cli
	@test -f "$(PM_ENV_FILE)" || { echo "missing environment file: $(PM_ENV_FILE)"; exit 1; }
	@cd "$(APP_DIR)" && go build -o ".bin/pm-dev" ./cmd/pm
	@set -a; . "$(PM_ENV_FILE)"; set +a; cd "$(APP_DIR)" && GENKIT_ENV=dev GENKIT_REFLECTION_PORT="$(GENKIT_REFLECTION_PORT)" GENKIT_ENABLE_REALTIME_TELEMETRY=true exec "$(abspath $(GENKIT_BIN))" start -- sh ./scripts/pm-dev-server.sh
