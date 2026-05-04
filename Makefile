SHELL=/bin/bash -o pipefail

.PHONY: all
all: build

.PHONY: build
build:
	go build ./...

.PHONY: test
test:
	go test ./...

.PHONY: test-ci
test-ci: testdb
	go test ./... -covermode=atomic -coverpkg=./... -coverprofile=./coverage.txt -json | tee output.txt

.PHONY: lint
lint:
	golangci-lint run ./...

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: testdb
testdb:
	cd $(shell go mod download -json github.com/smartcontractkit/chainlink/v2@4cb89093788a1c3acfc183fb86adbd722ebb6bfb | jq -r .Dir) && \
	go run ./core/store/cmd/preparetest
