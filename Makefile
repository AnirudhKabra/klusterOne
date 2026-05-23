BINARY := fury-controller
PKG    := github.com/fury/fury-controller
IMAGE  ?= fury-controller:dev

.PHONY: tidy build run test fmt vet install-crd uninstall-crd docker

tidy:
	go mod tidy

build:
	CGO_ENABLED=0 go build -o bin/$(BINARY) ./cmd/manager

run: build
	./bin/$(BINARY) --leader-elect=false

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

install-crd:
	kubectl apply -f config/crd/fury.io_nodemaintenances.yaml

uninstall-crd:
	kubectl delete -f config/crd/fury.io_nodemaintenances.yaml --ignore-not-found

docker:
	docker build -t $(IMAGE) .
