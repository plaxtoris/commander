FROM golang:1.27.4-bookworm AS toolchain
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git make && rm -rf /var/lib/apt/lists/* \
    && git config --system --add safe.directory /workspace
COPY --from=toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH
WORKDIR /workspace

# Match the host user so manual Git and parallel worktrees share file ownership.
RUN if ! getent group 1000 >/dev/null; then groupadd --gid 1000 dev; fi \
    && useradd --create-home --non-unique --uid 1000 --gid 1000 --shell /bin/bash dev
ENV PATH="/home/dev/.local/bin:${PATH}"

