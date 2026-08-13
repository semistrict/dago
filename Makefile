.PHONY: check checkpoint-interop coverage dacode-e2e drift fmt fmt-check generate test test-openai-live test-race tinygo vet

check: fmt-check drift vet test test-race

fmt:
	go fmt ./...

fmt-check:
	@test -z "$$(gofmt -l .)"

generate:
	go generate ./internal/conformance

drift:
	go run ./internal/conformance/cmd/generate -check
	sh scripts/check-upstream.sh

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

checkpoint-interop:
	./scripts/check-checkpoint-interop.sh
