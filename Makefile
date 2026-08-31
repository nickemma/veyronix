.PHONY: build test test-race vet verify run run-kubernetes render render-operator

build:
	mkdir -p bin
	go build -o bin/plinth ./cmd/plinth
	go build -o bin/plinthd ./cmd/plinthd
	go build -o bin/plinth-operator ./operator

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

verify: test test-race vet build render render-operator

run:
	go run ./cmd/plinthd --backend=fake

run-kubernetes:
	go run ./cmd/plinthd --backend=kubernetes --namespace=plinth-test

render:
	kubectl kustomize deploy

render-operator:
	kubectl kustomize operator
