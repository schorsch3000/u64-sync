# u64-sync developer tasks. `make help` lists them.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin
IMAGE   ?= u64-sync
CONFIG  ?= config.yaml

# Cross-build targets for `make dist`, as GOOS/GOARCH.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.DEFAULT_GOAL := help
.PHONY: help build probe run test test-short race cover vet fmt fmt-check tidy tidy-check check ci \
        cross dist docker docker-up docker-down clean

help: ## show this help
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

build: ## build bin/u64-sync (static, CGO off)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/u64-sync ./cmd/u64-sync

probe: ## build bin/u64-probe (inspects a device: FTP features, connection limit)
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(BIN)/u64-probe ./cmd/u64-probe

run: build ## run with $(CONFIG) (default config.yaml)
	$(BIN)/u64-sync -config $(CONFIG)

test: ## run all tests
	go test ./...

test-short: ## run tests, skip the slow end-to-end test
	go test $$(go list ./... | grep -v /e2e)

race: ## run all tests with the race detector (needs a C compiler)
	go test -race ./...

cover: ## tests with coverage summary; HTML report in bin/coverage.html
	@mkdir -p $(BIN)
	go test -coverprofile=$(BIN)/coverage.out ./...
	go tool cover -func=$(BIN)/coverage.out | tail -1
	go tool cover -html=$(BIN)/coverage.out -o $(BIN)/coverage.html

vet: ## go vet, also for windows and darwin
	go vet ./...
	GOOS=windows go vet ./...
	GOOS=darwin go vet ./...

fmt: ## format the code
	gofmt -w .

fmt-check: ## fail if code is not gofmt-formatted
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run: make fmt"; exit 1; }

tidy: ## go mod tidy
	go mod tidy

tidy-check: ## fail if go.mod / go.sum are not tidy
	go mod tidy && git diff --exit-code go.mod go.sum

check: fmt-check tidy-check vet race ## everything CI checks (formatting, tidy, vet, race tests)

ci: check

cross: ## build all release platforms into bin/ (no archives)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=""; [ $$os = windows ] && ext=".exe"; \
	  echo "build $$os/$$arch"; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" \
	    -o $(BIN)/u64-sync_$${os}_$${arch}$$ext ./cmd/u64-sync || exit 1; \
	done

dist: cross ## release archives (tar.gz, zip for windows) in bin/dist/
	@mkdir -p $(BIN)/dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; name=u64-sync_$(VERSION)_$${os}_$${arch}; \
	  rm -rf $(BIN)/dist/$$name; mkdir -p $(BIN)/dist/$$name; \
	  if [ $$os = windows ]; then exe=u64-sync.exe; src=$(BIN)/u64-sync_$${os}_$${arch}.exe; else exe=u64-sync; src=$(BIN)/u64-sync_$${os}_$${arch}; fi; \
	  cp $$src $(BIN)/dist/$$name/$$exe; cp README.md LICENSE config.example.yaml $(BIN)/dist/$$name/; \
	  if [ $$os = windows ]; then (cd $(BIN)/dist && zip -qr $$name.zip $$name); \
	  else tar -C $(BIN)/dist -czf $(BIN)/dist/$$name.tar.gz $$name; fi; \
	  rm -rf $(BIN)/dist/$$name; \
	done
	@cd $(BIN)/dist && sha256sum *.tar.gz *.zip > SHA256SUMS && ls -1

docker: ## build the docker image ($(IMAGE):$(VERSION) and :latest)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

docker-up: ## docker compose up -d --build (needs config.yaml and ./data)
	@mkdir -p data
	docker compose up -d --build

docker-down: ## docker compose down
	docker compose down

clean: ## remove build output
	rm -rf $(BIN)
