.PHONY: help test test-integration tidy lint vulncheck fix check-fix update-lint update-tools fmt vet check build clean install
.DEFAULT_GOAL := help

BINARY_NAME := turango
BUILD_DIR   := bin
CMD         := ./cmd/turango

## help: Show this help message
help:
	@echo "Available targets:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'

## test: Run the fast unit test suite (no real toolchain subprocesses)
test:
	@echo "Running tests..."
	@go test ./...

## test-integration: Run the full suite, including tests that shell out to the
## real Go toolchain (compiling/testing throwaway module copies; slow)
test-integration:
	@echo "Running tests, including integration tests..."
	@go test -tags=integration ./...

tidy:
	@echo "Tidying Go modules..."
	@go mod tidy

## lint: Run golangci-lint with auto-fix enabled
lint:
	@echo "Running golangci-lint..."
	@go tool -modfile=tools.mod golangci-lint run --fix ./...

## vulncheck: Check dependencies for known vulnerabilities
vulncheck:
	@echo "Checking dependencies for known vulnerabilities..."
	@go tool -modfile=tools.mod govulncheck ./...

## fix: Apply standard Go modernization rewrites
fix:
	@go fix ./...

## check-fix: Fail if standard Go modernization rewrites are needed
check-fix:
	@go fix -diff ./...

## update-lint: Update golangci-lint to latest version
update-lint:
	@echo "Updating golangci-lint..."
	@go get -tool -modfile=tools.mod github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

## update-tools: Update all development and CI tools
update-tools:
	@echo "Updating development and CI tools..."
	@go get -tool -modfile=tools.mod github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest golang.org/x/vuln/cmd/govulncheck@latest

## fmt: Format code
fmt:
	@echo "Formatting code..."
	@go fmt ./...

## vet: Run go vet
vet:
	@echo "Running go vet..."
	@go vet ./...

## check: Run all checks (format, vet, lint, test)
check: tidy fmt vet lint vulncheck check-fix test
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
