# NotiBridge — macOS notifications → Telegram
BINARY  := notibridge
# The version lives in internal/app/service.go; it is injected into the binary
# at link time and must match the release tag.
VERSION := $(shell sed -n 's/^var Version = "\(.*\)"$$/\1/p' internal/app/service.go)
ARCH    := arm64

.PHONY: build dev test vet fmt check release bundle package-amd64 package-universal icons version diagnose clean

version: ## Print the current version
	@echo $(VERSION)

build: ## Compile for the current platform
	go build -o bin/$(BINARY) ./cmd/notifyrelay

dev: build ## Run from the command line (development)
	./bin/$(BINARY) run -debug

test: ## Run all automated tests
	go test ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all sources
	gofmt -w .

check: fmt vet test ## Format, vet and test

diagnose: build ## Check Notification Center database access
	./bin/$(BINARY) diagnose

icons: ## Regenerate assets/icons from the project icon (image.png)
	go run ./scripts/make_icons

release: ## Release build into dist/
	mkdir -p dist
	GOOS=darwin GOARCH=$(ARCH) CGO_ENABLED=1 go build -trimpath \
	  -ldflags "-s -w -X github.com/spdeepak/notibridge/internal/app.Version=$(VERSION)" \
	  -o dist/$(BINARY) ./cmd/notifyrelay
	@echo "Built dist/$(BINARY)"

bundle: ## Assemble dist/NotiBridge.app and its release archive
	scripts/package_app.sh $(VERSION) $(ARCH)

package-amd64: ## Build the release archive for Intel Macs
	scripts/package_app.sh $(VERSION) amd64

package-universal: ## Build a universal (arm64 + amd64) release archive
	scripts/package_app.sh $(VERSION) universal

clean:
	rm -rf bin dist
