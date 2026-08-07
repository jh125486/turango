.PHONY: help test tidy lint update-lint fmt vet check build clean install
.DEFAULT_GOAL := help

BINARY_NAME := turango
BUILD_DIR   := bin
CMD         := ./cmd/turango

## help: Show this help message
help:
	@echo "Available targets:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'

## test: Run all tests
test:
	@echo "Running tests..."
	@go test ./...

tidy:
	@echo "Tidying Go modules..."
	@go mod tidy

## lint: Run golangci-lint with auto-fix enabled
lint:
	@echo "Running golangci-lint..."
	@go tool -modfile=tools/golangci-lint/go.mod golangci-lint run --fix ./...

## update-lint: Update golangci-lint to latest version
update-lint:
	@echo "Updating golangci-lint..."
	@go get -tool -modfile=tools/golangci-lint/go.mod github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

## fmt: Format code
fmt:
	@echo "Formatting code..."
	@go fmt ./...

## vet: Run go vet
vet:
	@echo "Running go vet..."
	@go vet ./...

## check: Run all checks (format, vet, lint, test)
check: tidy fmt vet lint test
	@echo "All checks completed"

## build: Build the binary
build:
	@go build -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD)

## clean: Remove build artifacts
clean:
	@rm -rf $(BUILD_DIR)/

## install: Install the binary
install:
	@go install $(CMD)
