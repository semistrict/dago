.PHONY: check checkpoint-interop coverage dacode-e2e drift fmt fmt-check generate module-floor openrouter-e2e t3-desktop test test-openai-live test-race tinygo vet

T3_DACODE_BIN ?= /tmp/dago-t3-desktop/dacode
T3_NODE_BIN_DIR ?= $(shell if test -x /opt/homebrew/opt/node@24/bin/node; then echo /opt/homebrew/opt/node@24/bin; else dirname "$$(command -v node)"; fi)

check: fmt-check drift module-floor vet test test-race

fmt:
	go fmt ./...

fmt-check:
	@test -z "$$(gofmt -l .)"

generate:
	go generate ./internal/conformance

drift:
	go run ./internal/conformance/cmd/generate -check
	sh scripts/check-upstream.sh

# Go release binaries contain one immutable selected module graph. Verify its
# checksums and reject a go.mod/go.sum pair below the source-declared floors.
module-floor:
	go mod verify
	go mod tidy -diff

vet:
	go vet ./...

test:
	go test ./...

test-openai-live:
	@test -n "$$DAGO_OPENAI_OAUTH_FILE" || (echo "DAGO_OPENAI_OAUTH_FILE must point to an existing OAuth JSON file"; exit 1)
	DAGO_OPENAI_LIVE=1 go test ./daproviders/openai -run '^TestLiveResponses.*OAuthEndToEnd$$' -count=1 -v

test-race:
	go test -race ./...

tinygo:
	tinygo build -o /dev/null ./examples/basic
	tinygo build -target=wasm -o /dev/null ./examples/basic

coverage:
	go test -coverprofile=coverage.out ./...

dacode-e2e:
	cd internal/dacode/xtermjs && pnpm test:e2e

t3-desktop:
	git submodule update --init t3code
	@PATH="$(T3_NODE_BIN_DIR):$$PATH" node -e 'if (process.versions.node.split(".")[0] !== "24") { throw new Error("T3 Code requires Node 24") }'
	cd t3code && PATH="$(T3_NODE_BIN_DIR):$$PATH" CI=true corepack pnpm install --frozen-lockfile
	mkdir -p "$(dir $(T3_DACODE_BIN))"
	go build -o "$(T3_DACODE_BIN)" ./cmd/dacode
	cd t3code && PATH="$(T3_NODE_BIN_DIR):$$PATH" CI=true corepack pnpm build:desktop
	cd t3code && PATH="$(dir $(T3_DACODE_BIN)):$(T3_NODE_BIN_DIR):$$PATH" corepack pnpm dev:desktop

checkpoint-interop:
	./scripts/check-checkpoint-interop.sh

openrouter-e2e:
	@if test -z "$$OPENROUTER_API_KEY"; then echo "OPENROUTER_API_KEY is required"; exit 1; fi
	DAGO_OPENROUTER_E2E=1 go test -count=1 -run '^TestOpenRouterLive$$' ./daproviders/openrouter
	$(MAKE) -C examples/shelley openrouter-e2e
