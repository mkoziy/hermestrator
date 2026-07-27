APP_DIR := app
PM_ENV_FILE ?= $(APP_DIR)/.env
GENKIT_REFLECTION_PORT ?= 3100

.PHONY: check fmt-check mod-check vet lint test race install-hooks pm-run pm-dev

check: fmt-check mod-check vet lint test race

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

install-hooks:
	@git config core.hooksPath .githooks

# pm-run loads local, ignored development settings only for the dashboard
# process. Production must provide its environment through its runtime.
pm-run:
	@test -f "$(PM_ENV_FILE)" || { echo "missing environment file: $(PM_ENV_FILE)"; exit 1; }
	@set -a; . "$(PM_ENV_FILE)"; set +a; cd "$(APP_DIR)" && exec go run ./cmd/pm

# pm-dev runs the Genkit Developer UI locally. It is diagnostic-only and must
# never be exposed as the authenticated operator dashboard.
pm-dev:
	@command -v genkit >/dev/null || { echo "install the pinned Genkit CLI from the project image or https://genkit.dev before running pm-dev"; exit 1; }
	@test -f "$(PM_ENV_FILE)" || { echo "missing environment file: $(PM_ENV_FILE)"; exit 1; }
	@set -a; . "$(PM_ENV_FILE)"; set +a; cd "$(APP_DIR)" && GENKIT_ENV=dev GENKIT_REFLECTION_PORT="$(GENKIT_REFLECTION_PORT)" exec genkit start -- go run ./cmd/pm
