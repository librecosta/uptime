.PHONY: fmt fmt-check vet test race build check

# Use the host's null device even when GOOS selects a cross-compilation target.
NULL_DEVICE := $(if $(filter windows,$(shell go env GOHOSTOS)),NUL,/dev/null)
GO_FILES := $(shell git ls-files --cached --others --exclude-standard -- "*.go")

fmt:
	gofmt -w $(GO_FILES)

# GNU Make checks the file list without depending on POSIX shell conditionals.
# The gofmt command also fails for invalid Go syntax or unreadable source.
fmt-check:
	$(if $(strip $(shell gofmt -l $(GO_FILES))),$(error Go source requires formatting; run make fmt))
	gofmt -d $(GO_FILES)

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./storage/bbolt/... ./internal/app/... ./internal/archive/... ./cmd/uptime/...

# Package verification only; no binary is written into the working tree.
build:
	go build -o $(NULL_DEVICE) ./...

check: fmt-check vet test build
