GO ?= go
VERSION ?= 0.1.0-dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race vet fmt-check cross offline verify clean

build:
	$(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o bin/meshcompute ./cmd/meshcompute

test:
	$(GO) test ./... -count=1

race:
	$(GO) test -race ./... -count=1

vet:
	$(GO) vet ./...

fmt-check:
	test -z "$$(gofmt -l .)"

cross:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -o /tmp/meshcompute-linux-amd64 ./cmd/meshcompute
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -o /tmp/meshcompute-linux-arm64 ./cmd/meshcompute
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -o /tmp/meshcompute-windows-amd64.exe ./cmd/meshcompute

offline:
	GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOWORK=off $(GO) build ./...
	GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOWORK=off $(GO) test ./... -count=1

verify: fmt-check vet test cross offline

clean:
	rm -rf bin
