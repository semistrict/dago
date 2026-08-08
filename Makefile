.PHONY: check checkpoint-interop coverage drift fmt fmt-check generate test test-race vet

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
	sh scripts/check-shelley-upstream-tests.sh

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

coverage:
	go test -coverprofile=coverage.out ./...

checkpoint-interop:
	./scripts/check-checkpoint-interop.sh
