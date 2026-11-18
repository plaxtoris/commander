GO ?= go
.PHONY: test build clean

test:
	$(GO) vet ./...
	$(GO) test ./...

# Each binary is named for the machine that uname -m prints.
compile = CGO_ENABLED=0 GOOS=linux GOARCH=$(1) $(GO) build -trimpath -ldflags="-s -w -X main.build=$$(git rev-parse HEAD)" -o dist/commander-$(2) ./src

build:
	$(call compile,amd64,x86_64)
	$(call compile,arm64,aarch64)

clean:
	rm -rf dist
