# ── variables ──────────────────────────────────────────────────────────────────
BINARY     := build/miroxy
IMAGE      := forrestisagoodman/miroxy
VERSION    ?= dev
CONTAINER  := miroxy
CONFIG     := config/config.yaml
SECRETS    := config/secrets.env
PROXY_PORT ?= 9000
ADMIN_PORT ?= 9001

LDFLAGS := -trimpath -buildvcs=false -ldflags="-s -w -X main.version=$(VERSION)"

.PHONY: gen gen-check \
        build clean test lint install \
        run \
        docker-build docker-push docker-run docker-stop docker-logs docker-sh \
        help

# ── local build ────────────────────────────────────────────────────────────────

## ── Code generation ──────────────────────────────────────────────────────────
## Run after modifying docs/api/admin-openapi.yaml or any .proto file.
## Requires: go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
OAPI_CODEGEN := go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest

gen:                            ## Regenerate code from specs (no install required)
	$(OAPI_CODEGEN) --config internal/api/oapi-codegen.yaml internal/api/admin-openapi.yaml

gen-check:                      ## Verify generated files match specs (CI use)
	$(OAPI_CODEGEN) --config internal/api/oapi-codegen.yaml internal/api/admin-openapi.yaml
	git diff --exit-code -- '*.gen.go' '*.pb.go'

## ── Build ─────────────────────────────────────────────────────────────────────
build:                          ## Build binary to build/miroxy
	mkdir -p build
	go build $(LDFLAGS) -o $(BINARY) ./cmd/miroxy
	@echo "built: $(BINARY)  ($$(du -sh $(BINARY) | cut -f1))"

install: build                  ## Copy binary to current directory as ./miroxy
	cp $(BINARY) ./miroxy
	@echo "installed: ./miroxy"

clean:                          ## Remove build/ and ./miroxy
	rm -rf build/ miroxy

test:                           ## Run full test suite
	go test ./...

lint:                           ## Run golangci-lint (install separately)
	golangci-lint run ./...

# ── local run ──────────────────────────────────────────────────────────────────

run: build                      ## Build and run locally (loads secrets.env)
	@if [ ! -f $(SECRETS) ]; then \
		echo "error: $(SECRETS) not found — copy from $(SECRETS).example and fill in keys"; \
		exit 1; \
	fi
	env $$(grep -v '^#' $(SECRETS) | grep -v '^$$' | xargs) \
		./$(BINARY) serve -c $(CONFIG)

# ── docker ─────────────────────────────────────────────────────────────────────

docker-build:                   ## Build docker image  (IMAGE=... VERSION=... to override)
	docker build -t $(IMAGE):$(VERSION) .
	@echo "image: $(IMAGE):$(VERSION)"

docker-push: docker-build       ## Build and push to Docker Hub
	docker push $(IMAGE):$(VERSION)

docker-run:                     ## Run container (stops existing one first)
	@if [ ! -f $(SECRETS) ]; then \
		echo "error: $(SECRETS) not found"; exit 1; \
	fi
	-docker rm -f $(CONTAINER) 2>/dev/null || true
	docker run -d \
		--name $(CONTAINER) \
		--env-file $(SECRETS) \
		-v "$(shell pwd)/$(CONFIG):/app/config/config.yaml:ro" \
		-p $(PROXY_PORT):9000 \
		-p $(ADMIN_PORT):9001 \
		$(IMAGE):$(VERSION)
	@echo "started $(CONTAINER)  proxy=:$(PROXY_PORT)  admin=:$(ADMIN_PORT)"

docker-stop:                    ## Stop and remove container
	docker rm -f $(CONTAINER)

docker-logs:                    ## Tail container logs
	docker logs -f $(CONTAINER)

docker-sh:                      ## Open shell inside running container
	docker exec -it $(CONTAINER) sh

# ── help ───────────────────────────────────────────────────────────────────────

help:                           ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*##"}; {printf "  %-18s %s\n", $$1, $$2}'
