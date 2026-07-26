APP_DIR := app

.PHONY: check fmt-check mod-check vet lint test race install-hooks

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
