FROM golang:1.27.4-bookworm AS toolchain
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git make && rm -rf /var/lib/apt/lists/* \
    && git config --system --add safe.directory /workspace
COPY --from=toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH
WORKDIR /workspace
