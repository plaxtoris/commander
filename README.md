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

## Shared agent settings

Use `commander config codex config.toml`, `commander config claude settings.json`
or `commander config pi settings.json` to set defaults for all sessions. Omit
the file to print the saved settings. Use `-` to read from standard input. An
empty file clears the saved settings for that agent.

Commander stores these preferences in `.secrets/coding-config.json` in its
installation directory. New launches load the shared settings. When an agent
exits, Commander saves changes it made to its native settings for future
launches. If another session or the config command has already changed that
agent's shared settings, Commander keeps the newer version and reports a
conflict. Logins are selected from the current session. Claude loads its
shared user preferences alongside project settings. To adopt settings from
an existing Claude account profile, import its user settings with
`commander config claude FILE`.

For Pi, set `defaultModel` to a model ID.
Commander uses the first model from the selected endpoint if it cannot find that
ID.

Encrypted exports automatically include all saved agent settings. Import replaces
the settings for each agent included in the archive and keeps the settings for
other agents. Use the current Commander version to open archives with settings.
Use these files for preferences and manage logins through the Commander menu.

## Update cleanup

After a successful update check, Commander removes abandoned update downloads.
Private runtime directories for agents and tools live under `.secrets/.runtime`
and are removed when their commands exit. If a command was interrupted, the
updater removes its marked runtime directory after the next system boot. It
also removes marked tool installation staging directories from earlier boots.

The first run of this version records the current boot in a filesystem
attribute. After the next boot, Commander uses one shared lock file for
installation operations and removes the old empty lock files and recognizable
legacy runtime directories during an update check. On filesystems without
extended attributes it keeps the legacy locks. Interrupted imports are kept
for recovery. Session command wrappers are shared by all sessions.

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
