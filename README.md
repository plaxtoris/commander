# Commander

Commander gives your AI agent a portable workspace to get work done across your
entire development stack.

> “Implement the feature across three services and prepare the cloud infrastructure
> for release.”

Connect GitHub, Docker Hub, Microsoft 365, Azure, Google Cloud, Hetzner and
Confluence. Your agent coordinates work across repositories and infrastructure
with native tools and clearly scoped accounts.

Your agent learns how you work through a small, adaptive memory and tells you
when it changes. Keep your preferences when you switch agents. Tools install
when needed. Commander keeps itself up to date.

## Install and run

```bash
mkdir -p ~/commander && cd ~/commander &&
curl -fLO https://github.com/plaxtoris/commander/releases/download/latest/commander &&
chmod +x commander && ./commander
```

Connect your accounts in the menu. Start your preferred AI agent in the same
folder. Describe what you want done.

For WSL and Linux on x86_64. Project containers need Docker Engine or Docker Desktop.

## Development

Keep Commander small and independent of any agent. Source and tests: `src/`.
Tool pins: `src/tools.go`. Client instructions: `src/AGENTS.template.md`.
Personal `MEMORY.md` survives updates. Memory is English; replies follow the user.

Use the devcontainer. Run `make test` and `make build`. Check setup, relocation and
updates when affected; test container changes in a disposable container.
Use authentication fixtures. Never read real credentials or manage agent logins.
Verify identity before service work.

Use short, direct English without em dashes or hyphenated prose. CI publishes
`commander` and `SHA256SUMS` to `latest`. Preserve repository visibility.
Run reset helpers only when requested.
