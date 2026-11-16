FROM golang:1.27.4-bookworm AS toolchain
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git gh make procps \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 --shell /bin/bash vscode

COPY --from=toolchain /usr/local/go /usr/local/go
ENV PATH="/home/vscode/.local/bin:/usr/local/go/bin:${PATH}"
USER vscode

RUN curl -fsSL https://chatgpt.com/codex/install.sh -o /tmp/codex-install.sh \
    && CODEX_NON_INTERACTIVE=1 sh /tmp/codex-install.sh </dev/null \
    && curl -fsSL https://claude.ai/install.sh -o /tmp/claude-install.sh \
    && bash /tmp/claude-install.sh </dev/null \
    && rm /tmp/codex-install.sh /tmp/claude-install.sh \
    && codex --version \
    && claude --version \
    && ps -p "$$" -o pid=

WORKDIR /workspaces/commander
