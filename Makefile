.PHONY: build test run lint lint-fix mod-tidy golangci-lint modernize govulncheck

build:
	go build -o gitonce .

test: build
	go test -race -coverprofile=cover.out ./...
	go tool cover -func=cover.out | tail -1

run: build
	./gitonce

lint: mod-tidy golangci-lint modernize govulncheck

lint-fix:
	go mod tidy
	golangci-lint run --fix
	go fix ./...
	$(MAKE) lint

mod-tidy:
	go mod tidy -diff

golangci-lint:
	golangci-lint run

modernize:
	go fix -diff ./... | awk '{print} /\S/ {found=1} END {if (found) exit 1}'

govulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
