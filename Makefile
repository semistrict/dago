.PHONY: check checkpoint-interop coverage drift fmt fmt-check generate test test-race tinygo vet

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

test-race:
	go test -race ./...

tinygo:
	tinygo build -o /dev/null ./examples/basic
	tinygo build -target=wasm -o /dev/null ./examples/basic

coverage:
	go test -coverprofile=coverage.out ./...

checkpoint-interop:
	./scripts/check-checkpoint-interop.sh
