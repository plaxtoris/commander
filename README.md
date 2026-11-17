# Commander

A portable workspace for native tools, accounts, agents and repositories.
Start your preferred agent independently. Tools install when needed.

```sh
mkdir -p ~/commander && cd ~/commander
curl -fLO https://github.com/plaxtoris/commander/releases/download/latest/commander
chmod +x commander
./commander
```

Connect accounts, create a session, select its accounts and repositories, then
open its shell. Existing profiles are optional saved selections. A GitHub login
and its allowed owners or repositories are selected separately.

| Command | Purpose |
| --- | --- |
| `./commander session new NAME` | Select accounts and repositories |
| `./commander session shell ID` | Open an independent session shell |
| `./commander context` | Show the current selection |
| `./commander exec -- TOOL ARGS` | Use selected credentials |
| `./commander up PROJECT` | Build and start a trusted project container |
| `./commander container ID -- COMMAND` | Run inside that container |
| `./commander task new PROJECT NAME main` | Create an isolated worktree |
| `./commander task finish NAME -- make test` | Check, integrate and clean up |
| `./commander export` / `./commander import FILE.zip` | Select accounts, presets and optional memory |
| `./commander memory` | Read shared preferences |
| `./commander session close ID` | Remove that session and its finished containers |

Keep projects in `workspaces/<owner>/<repo>/`. Read their README. Concurrent tasks
use separate worktrees; completion checks the integrated result before removing
temporary branches. Failed work remains available. Selections guide trusted tools;
they do not reduce provider token permissions or authorize actions.

[Usage, account isolation, container Git and memory](docs/usage.md).

The development container runs as root in `/workspace`. Use `make test` and
`make build`. Source lives in `src/`. CI also tests races and real containers,
then publishes one rolling `latest` release. Never run destructive reset helpers
for ordinary development.
