# Commander

Commander is a portable workspace for command line tools, accounts, projects and AI agents.
It runs on WSL and Linux on x86_64. Bring your preferred agent.

Commander combines `AGENTS.md` and `MEMORY.md` into an adaptive, self learning system.
The instructions guide your agent. Memory carries your lasting preferences across
sessions and agents. Your active agent learns and consolidates them, then briefly
reports each saved change. Commander protects the file and its 4000 character limit.

## Start

```bash
mkdir -p ~/commander && cd ~/commander &&
curl -fLO https://github.com/plaxtoris/commander/releases/download/latest/commander &&
chmod +x commander && ./commander
```

Choose a profile, add accounts, then start your agent in the Commander folder.
Profiles define account scope. Tools install when needed and are shared across
profiles. GitHub, Azure, Microsoft 365, Hetzner, Google Cloud, Docker Hub and
Confluence Cloud are supported.

## Use

```bash
./commander                    # Open the menu and check for updates
./commander context            # Show the current account scope
./commander exec -- gh auth status
./commander up PROFILE/REPO    # Start a trusted project container
./commander memory             # Read personal preferences
./commander help               # List all commands
```

Prefer `workspaces/<profile-name>/<repo-name>/` for new repositories. From project
folders, use the absolute Commander path. Install Docker Engine or Docker Desktop
separately for containers. Use `doctor` to check tools, `setup` to repair them,
and `export` or `import` for encrypted transfers of selected account logins.
Credentials stay in `.secrets/`. Never commit them.

## Memory and updates

Memory is a short English bullet list of personal preferences shared across your
profiles. Replies follow the language of your prompt unless you request another.
The agent updates memory when you express or correct a lasting preference.
It removes duplicates and obsolete statements on each update. Project details,
temporary requests and secrets stay out.

Memory is created on the first saved change and ignored by Git. Writes above
4000 Unicode characters, including spaces and newlines, are rejected. Conflicting
writes are rejected too. The agent reads again, consolidates and retries.
There is no background AI service.

Menu startup checks the latest release and restarts after an update. A failed
update check leaves the menu available. Menu startup and `setup` refresh
`AGENTS.md`, replacing local edits, while preserving `MEMORY.md`.
Direct commands do not update the executable; `update` does.

## Develop

This README guides contributors and development agents. Keep Commander small,
generic and independent of any agent. Code and tests live in `src/`; tool pins
live in `src/tools.go`. `src/AGENTS.template.md` is embedded in the executable
and generates instructions for installed clients. Keep memory rules there.
Personal memory belongs to client installations and must survive updates.

Open the VS Code devcontainer. Run `make test` and `make build`. Check setup,
relocation and updates when affected. Test container changes in a disposable
container. Use local authentication fixtures. Never read real credentials or
manage agent logins. Verify identity before service work.

Write short, direct English documentation without em dashes or hyphenated prose.
GitHub Actions checks changes and publishes `commander` and `SHA256SUMS` under
the single rolling `latest` release. Preserve repository visibility. Run repository
reset helpers only when the user requests a reset.
