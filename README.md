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

Open the menu with `./commander`, or see all commands with `./commander help`.
Each menu start checks the rolling `latest` release. A changed executable is
downloaded, checksum-verified and installed atomically, then Commander restarts
automatically and prepares the tools required by the new version. Missing tools
and changed tool versions are installed automatically; ready tools are reused.
The update check times out after eight seconds. If checking or downloading fails,
Commander continues with the installed version. An incomplete tool installation
is retried on the next start and still needs network access.

The menu has three entries: **Accounts**, **Profiles** and **Quit**.
Accounts shows a table of your selected logins and their status, with actions to
add, remove or refresh them. Profiles handles switching, creating, importing
and exporting profiles. No Update or Setup menu entry is needed.
Installation diagnostics remain available through `./commander doctor`;
`./commander setup` resumes an incomplete installation. `./commander update`
remains available for a manual update. Other commands, including `exec`, do not
trigger automatic updates. Private release downloads require the active GitHub
identity to belong to the current profile; Commander verifies it without switching
accounts.

Use arrow keys or Tab to move, Enter to select, and Esc to go back. Numbers jump
to a menu entry; Page Up / Page Down scroll long account overviews. Set `NO_COLOR=1`
for a plain appearance.

Docker Engine or Docker Desktop with WSL
integration is needed for project containers. Docker images and volumes stay
in Docker storage when you move the folder.

## Develop

Open this repository in its VS Code devcontainer. Code and tests live in `src/`.
Run `make test` to check changes and `make build` to build `dist/commander`.
GitHub Actions tests, builds and replaces the single `latest` release.
