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

The first start opens the menu without installing service tools. Choose a profile
and add an account: Commander prepares only that service's CLI when you select it.
GitHub, Azure, Microsoft 365, Hetzner, Google Cloud, Docker Hub and Confluence Cloud
are available. Tools are shared by all accounts; project container tools are
prepared on first use. Then start your preferred agent in the same folder.
Commander creates `AGENTS.md` so your agent knows how to use it.

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
automatically and updates only tools already installed or requested. Ready tools
are reused. Microsoft 365 and project containers install independently, sharing
Node only when both need it.
The update check times out after eight seconds. If checking or downloading fails,
Commander continues with the installed version. An incomplete tool installation
is retried on the next start or use and still needs network access. A failed tool
installation leaves the menu available. Short progress messages go to stderr;
native command output and exit codes are preserved.

The menu has three entries: **Accounts**, **Profiles** and **Quit**.
Accounts shows a table of your selected logins and their status, with actions to
add, remove or refresh them. Profiles handles switching, creating, importing
and exporting profiles. No Update or Setup menu entry is needed.
Installation diagnostics remain available through `./commander doctor`;
`./commander setup` repairs used tools without adding unused services. Unused tools
are normal in `doctor`, not installation errors. `./commander update` remains
available for a manual update. Other commands, including `exec`, do not update the
Commander executable; `exec` prepares the requested tool at its pinned version.
Imported accounts prepare their tools when first checked or used.
Private release downloads require the active GitHub
identity to belong to the current profile; Commander verifies it without switching
accounts.

Use arrow keys or Tab to move, Enter to select, and Esc to go back. Numbers jump
to a menu entry; Page Up / Page Down scroll long account overviews. Set `NO_COLOR=1`
for a plain appearance.

Git is a native system command; install it with your system package manager when
needed. Docker Engine or Docker Desktop with WSL
integration is needed for project containers. Docker images and volumes stay
in Docker storage when you move the folder.

## Docker Hub and Confluence

Choose **Accounts → Add account → Docker Hub** or **Confluence Cloud**.
Enter your Docker username and personal access token, or your Confluence site URL,
Atlassian email and API token. Token input is hidden. Commander verifies the
account before saving it privately in `.secrets/`. Confluence supports classic and
scoped Cloud tokens, detecting the scoped API gateway automatically. Tokens need
permission to read the current user and to perform the requested API operations.
See [Docker tokens](https://docs.docker.com/security/access-tokens/)
and [Confluence tokens](https://support.atlassian.com/confluence/kb/scoped-api-tokens-in-confluence-cloud/).

Both small CLIs are built into Commander and require no extra downloads:

```bash
./commander exec -- dockerhub whoami
./commander exec -- dockerhub api /v2/namespaces/USERNAME/repositories
./commander exec -- confluence whoami
./commander exec -- confluence api '/api/v2/pages?limit=10'
./commander exec -- confluence api /api/v2/pages/123 -X PUT --input page.json
```

API responses go directly to stdout. Use `--input -` for stdin and `--help` for
options. Both CLIs use only the current profile and verify identity before each
request. With multiple accounts, pass the `--account` value shown by
`./commander context`.

Native `docker`, `docker-compose` and `commander up` use the profile's Docker Hub
account through a temporary private Docker configuration. No Hub account means
anonymous registry access. With several Hub accounts, choose one explicitly:

```bash
./commander exec -- dockerhub docker --account USERNAME -- push USERNAME/IMAGE:TAG
```

The temporary configuration is removed after the command. Token accounts are
included in the existing encrypted profile export and import. Removing a saved
token forgets it locally; revoke it at the provider if required.

## Develop

Open this repository in its VS Code devcontainer. Code and tests live in `src/`.
The image installs native [Codex](https://developers.openai.com/codex/cli) and
[Claude Code](https://code.claude.com/docs/en/setup) CLIs with their official
`curl` installers as `vscode`, without Node.js or npm. Both commands are on `PATH`;
`procps` supplies `ps` for Codex's background server. Installation is noninteractive.
After changing the image, run **Dev Containers: Rebuild Container** in VS Code,
then start `codex` or `claude` in a new terminal.

Run `make test` to check changes and `make build` to build `dist/commander`.
GitHub Actions tests, builds and replaces the single `latest` release.
