FROM golang:1.27.4-bookworm AS toolchain
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git gh make \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 --shell /bin/bash vscode

COPY --from=toolchain /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
USER vscode
WORKDIR /workspaces/commander
