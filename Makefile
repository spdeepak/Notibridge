# NotiBridge — macOS notifications → Telegram
BINARY  := notibridge
VERSION := 0.1.0

.PHONY: build dev test vet fmt check release bundle clean diagnose

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

release: ## Release build into dist/
	mkdir -p dist
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 go build -trimpath \
	  -ldflags "-s -w -X github.com/spdeepak/notibridge/internal/app.Version=$(VERSION)" \
	  -o dist/NotiBridge ./cmd/notifyrelay
	@echo "Built dist/NotiBridge"

bundle: release ## Assemble dist/NotiBridge.app
	scripts/package_app.sh $(VERSION)

clean:
	rm -rf bin dist
