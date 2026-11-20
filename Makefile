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
	ln -sfn commander-$$(uname -m) dist/commander
	@if [ "$$(id -u)" = 0 ]; then chown -h --reference=. dist dist/commander dist/commander-x86_64 dist/commander-aarch64; fi

clean:
	rm -f dist/commander dist/commander-x86_64 dist/commander-aarch64
