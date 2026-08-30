.PHONY: build test vet run

build:
	go build ./cmd/plinth ./cmd/plinthd

test:
	go test ./...

vet:
	go vet ./...

run:
	go run ./cmd/plinthd --backend=fake
