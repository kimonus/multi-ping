TARGETS = linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

# What -version prints: the latest tag, plus the commit when built past it.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null)
LDFLAGS = -X main.version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o multi-ping .

test:
	go vet ./... && go test ./...

# Cross-compile every target into a fresh dist/, with their checksums, so the
# directory never holds binaries of an older version.
dist:
	@rm -rf dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "$$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w $(LDFLAGS)" \
			-o dist/multi-ping-$$os-$$arch$$ext . || exit 1; \
	done
	@cd dist && if command -v sha256sum >/dev/null; then sha256sum multi-ping-*; \
		else shasum -a 256 multi-ping-*; fi > SHA256SUMS
	@echo "dist/: $(VERSION)"

.PHONY: build test dist
