.PHONY: build test race lint tidy

build:
	go build -o bin/delegate ./cmd/delegate

test:
	go test ./...

race:
	go test -race ./...

lint:
	go vet ./...

tidy:
	go mod tidy
