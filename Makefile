GO ?= go
.PHONY: test build clean

test:
	$(GO) vet ./...
	$(GO) test ./...

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w -X main.build=$$(git rev-parse HEAD)" -o dist/commander ./src

clean:
	rm -rf dist
