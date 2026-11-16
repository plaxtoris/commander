# Commander

Your tools, accounts and projects in one portable folder. For WSL and Linux on x86_64.

## Start

```bash
mkdir -p ~/commander && cd ~/commander &&
curl -fLO https://github.com/plaxtoris/commander/releases/download/latest/commander &&
chmod +x commander && ./commander
```

The first start opens the menu without installing service tools. Choose a profile
and add an account: Commander prepares only that service's CLI when you select it.
GitHub, Azure, Microsoft 365, Hetzner, Google Cloud, Docker Hub and Confluence Cloud
are available. Tools are shared by all accounts; project container tools are
prepared on first use. Then start your preferred agent in the same folder.
Commander creates `AGENTS.md` so your agent knows how to use it. For new
repositories, prefer `workspaces/<profile-name>/<repo-name>/` where practical.
Existing repository locations remain valid.

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
Menu startup, `setup` and manual updates regenerate the installation's `AGENTS.md`
from the running executable's template, replacing local edits. After an update,
the restarted version supplies the new instructions. Keep project-specific
instructions in each repository's own `AGENTS.md`.
The update check times out after eight seconds. If checking or downloading fails,
Commander continues with the installed version. An incomplete tool installation
is retried on the next start or use and still needs network access. A failed tool
installation leaves the menu available. Short progress messages go to stderr;
native command output and exit codes are preserved.

The menu has three entries: **accounts**, **profiles** and **quit**.
Accounts shows a table of your selected logins and their status, with actions to
add, remove or refresh them. Profiles handles switching, creating, importing
and exporting profiles. No Update or Setup menu entry is needed.
Installation diagnostics remain available through `./commander doctor`;
`./commander setup` repairs used tools without adding unused services. Unused tools
are normal in `doctor`, not installation errors. `./commander update` remains
available for a manual update. Other commands, including `exec`, do not update the
Commander executable; `exec` prepares the requested tool at its pinned version.
Importing a profile prepares Git and only the service CLIs used by its accounts,
including their Node/Python dependencies, then checks the account status. Docker
Hub and Confluence are built in. The profile is saved before preparation starts;
an interrupted download or a login needing attention does not undo the import.
Retry with `./commander setup PROFILE` without importing the archive again.
`./commander setup` without a profile still repairs only previously used tools.
Private release downloads require the active GitHub
identity to belong to the current profile; Commander verifies it without switching
accounts.

Use arrow keys or Tab to move, Enter to select, and Esc to go back. Numbers jump
to a menu entry; Page Up / Page Down scroll long account overviews. Set `NO_COLOR=1`
for a plain appearance. Interface labels use lowercase; account names, profile
names and contexts keep their original spelling. Cyan marks the current action,
green marks available logins, and amber marks accounts needing attention. Symbols
and text carry the same meaning without color. Account tables wrap long values
and switch to compact stacked rows on narrow terminals. Redirected output and
`TERM=dumb` stay free of color codes.

Commander reuses system Git. If Git is missing during import, GitHub CLI use or
project startup, it installs Git and CA certificates using apt-get, dnf, yum,
zypper, apk or pacman. Only the package manager uses `sudo` when necessary; run
Commander as your normal user. In a noninteractive terminal, missing sudo access
produces a retry instruction instead of waiting for a password. Unsupported
package managers require a manual Git installation. System Git stays on the
machine when you move the Commander folder and is checked again on the new host.

Docker Engine or Docker Desktop with WSL integration is a separate prerequisite
for project containers. Import checks engine access when a Docker CLI is already
available and otherwise reports that it could not check it. This does not block
service accounts. Commander installs Docker CLI, Compose and Devcontainer CLI on
first project use, but never installs the engine, starts a system service or
changes Docker permissions. Docker images and volumes stay in Docker storage
when you move the folder.

## Docker Hub and Confluence

Choose **accounts → add account → dockerhub** or **confluence**.
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
The development image contains Go, Git and Make. Agents run on the host.
Build and module caches stay in the ignored `.data/` directory.

Run `make test` to check changes and `make build` to build `dist/commander`.
GitHub Actions tests, builds and replaces the single `latest` release.
