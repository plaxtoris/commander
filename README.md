# Commander

Your tools, accounts and projects in one portable folder. For WSL and Linux on x86_64.

## Start

```bash
mkdir -p ~/commander && cd ~/commander &&
curl -fLO https://github.com/plaxtoris/commander/releases/download/latest/commander &&
chmod +x commander && ./commander
```

While this repository is private, download `commander` from
[latest](https://github.com/plaxtoris/commander/releases/tag/latest) while signed in.

The first start installs the tools for GitHub, Azure, Microsoft 365, Hetzner,
Google Cloud and project devcontainers. Choose a profile and add your logins.
Then start your preferred agent in the same folder. Commander creates `AGENTS.md`
so your agent knows how to use it.

```text
~/commander/
  commander
  AGENTS.md
  profiles.json
  .secrets/     # Logins
  tools/        # Installed tools
  workspaces/   # Your repositories and devcontainers
```

Open the menu with `./commander`, update with `./commander update`, or see all
commands with `./commander help`. Docker Engine or Docker Desktop with WSL
integration is needed for project containers. Docker images and volumes stay
in Docker storage when you move the folder.

## Develop

Open this repository in its VS Code devcontainer. Code and tests live in `src/`.
Run `make test` to check changes and `make build` to build `dist/commander`.
GitHub Actions tests, builds and replaces the single `latest` release.
