# Commander

One folder for your accounts, tools and repositories, ready for any coding agent.

Choose a session and press enter to open Codex, Claude Code or Pi in its
workspace. Each session defines the accounts and repositories available to the
agent. Agents can read any public Git repository, such as Claude plugins, but
push only to the session repositories. Each launch opens a new chat. In Codex
or Claude Code, use `/resume` to reopen a local conversation from the same
Commander session and agent account, even after a reboot. If you renamed the
session, use `codex resume --all` in its shell to include the old workspace.

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
  logins, organizations and repositories. Codex starts with approvals and sandboxing disabled. Pi trusts project files
  for the run. Claude uses bypass permissions as a regular user and requests
  auto mode under root, where its CLI rejects bypass permissions.
- **Edit.** Connect more accounts or change the scope. Choose a GitHub login
  per repository or organization when several can reach it. Changes save
  immediately. Press `r` in the editor to rename the session.
- **Startup.** Each normal launch checks for a Commander release in the background.
  Open windows share concurrent checks and check again hourly. A verified download
  shows `update ready  [r] restart` below the build time on the home screen.
  Press `r` to restart this window, or let the next normal launch apply the
  update. Downloads continue after the menu closes. Open chats keep running.
  The build date and time use Berlin time. Installed tools are checked hourly.
  Use `commander update` to check and apply a release immediately.
- **Accounts.** Commander checks session accounts in the background. Press
  `l` on the home screen to jump to a problem account.
  Its detail lists the sessions using it. Press enter to sign in again for a
  rejected login or retry an inconclusive check. For a missing provider account
  with no direct reconnect, enter opens the session editor. Press `c` on the
  home screen to check all session accounts.
- **Delete.** Press `d` on a session and confirm. To open a shell, run
  `commander session shell NAME`.
- **Transfer.** Press `x` to choose sessions and `MEMORY.md` for the encrypted
  `commander.zip` archive. Press `i` to choose a ZIP from the Commander folder
  or enter a path. The archive replaces local sessions with matching names
  and replaces `MEMORY.md` if included.
  `commander export SESSION` also includes existing memory and shared agent settings.
  After importing a Claude subscription on a different machine, sign in again
  through Claude Code.
- **Portable.** Commander installs the official CLIs on demand in its tools
  directory. The background check also refreshes installed tools. Active agent
  updates wait until the agent is idle. Pi lives in `tools/pi/`.

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

Claude keeps inactive conversations for 36,500 days by default. Set
`cleanupPeriodDays` in its shared settings to choose a shorter retention period.
Chat history stays on this machine when you export a session.

Pi remembers the last model and thinking level you chose in each Commander
session, separately for each endpoint. Changes take effect immediately for the
next chat. For a new session or an unavailable model, Commander uses the
shared `defaultModel` if available, then the first model from the endpoint.
Commander reads context and output limits from the endpoint's model catalog and
optional LiteLLM model information. For llama.cpp it uses the configured serving
window even when a model is unloaded. In Pi the footer shows the context window
and the output limit for the current request. `context unknown` means the
endpoint did not publish its window. Pi still uses its internal context estimate.
With `output server default`, the server sizes the reply. Commander uses this
when no separate output limit is known or Pi’s context reserve would leave a
single token under a larger server limit. An explicit output cap stays in force.

Use `/thinking` to select the effort on a model that advertises reasoning.
Commander also checks the templates of loaded llama.cpp models. On supported
templates the thinking budget shares the output allowance with the answer.
If the server ends a reply at its length limit, Pi shows the requested limit
and any reported output count. Tool output previews have their own size limits.

For a catalog with missing metadata, add the known values to your existing
Pi settings with `commander config pi settings.json`. Use the endpoint account
name and the exact model ID. Each field is optional. Published limits take
precedence when smaller.

```json
{
  "endpointModels": {
    "local": {
      "my-model": { "contextWindow": 32768, "maxTokens": 8192, "reasoning": true }
    }
  }
}
```

Commander includes `pi-token-speed` in its release. Every new Pi chat shows
tokens per second in the status bar during a reply and the average when it
finishes. Use `/tps` in Pi to customize the display.

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
Installed tools update in the background. Rebuild to update Commander itself.
The source lives in `src/`. Agent guidance comes from `AGENTS.template.md`,
the command reference from `commander.go` and tool pins from `tools.go`.
Use synthetic login fixtures for tests.
CI tests races and real containers on x86_64 and aarch64, publishes a fresh
`latest` release for every build of `main` and installs it on both.
