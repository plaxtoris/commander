# Commander

One folder for your accounts, tools and repositories, ready for any coding agent.

Choose a session and press enter to open Codex, Claude Code or Pi in its
workspace. Each session defines the accounts and repositories available to the
agent. Every launch starts a fresh conversation.

```sh
mkdir -p ~/commander && cd ~/commander
curl -fL -o commander https://github.com/plaxtoris/commander/releases/download/latest/commander-$(uname -m)
chmod +x commander && ./commander
```

```
  commander                                                      2 sessions
  ──────────────────────────────────────────────────────────────────────────

  › client      claude · work
    personal    codex · personal
    + new session

    github         alice
    repositories   acme/*

  [enter] start  [e] edit  [d] delete  [x] export  [i] import  [q] quit
```

- **Start.** One agent account opens directly. With several, pick the login
  you want. An OpenAI compatible endpoint opens in Pi. Model, effort and
  keyboard shortcuts belong to the native agent. Exiting returns to Commander.
- **New session.** Name it, highlight an agent account and press enter to start.
  Use space to select several agents. Open `accounts & repos` for GitHub
  logins, organizations and repositories. Agents run autonomously in this workspace.
- **Edit.** Connect more accounts or change the scope. Choose a GitHub login
  per repository or organization when several can reach it. Changes save
  immediately. Press `r` in the editor to rename the session.
- **Startup.** Commander updates the installed tools and checks session accounts
  in the background. Login issues show on the session that needs attention.
- **Delete.** Press `d` on a session and confirm. To open a shell, run
  `commander session shell NAME`.
- **Transfer.** Press `x` to choose sessions and `MEMORY.md` for the encrypted
  `commander.zip` archive. Press `i` to choose a ZIP from the Commander folder
  or enter a path. The archive replaces local sessions with matching names
  and replaces `MEMORY.md` if included.
  After importing a Claude subscription on a different machine, sign in again
  through Claude Code.
- **Portable.** Commander installs the official CLIs on demand in its tools
  directory and checks for updates on startup. Pi lives in `tools/pi/`.

Linux and WSL on x86_64 and aarch64 (arm64). Project containers need Docker Engine
or Docker Desktop.

## Development

Open this repository in its development container and run `make test` and
`make build`, which writes one binary per machine to `dist/`. Run
`cd dist && ./commander` to try the build with its own sessions and logins.
Installed tools update on startup. Rebuild to update Commander itself.
The source lives in `src/`. Agent guidance comes from `AGENTS.template.md`,
the command reference from `commander.go` and tool pins from `tools.go`.
Use synthetic login fixtures for tests.
CI tests races and real containers on x86_64 and aarch64, publishes a fresh
`latest` release for every build of `main` and installs it on both.
