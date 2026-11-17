# Commander

A portable workspace for agents, native tools, accounts and repositories.
Connect GitHub, Docker Hub, Microsoft 365, Azure, Google Cloud, Hetzner and
Confluence. Start your preferred agent yourself. Tools install when needed.

## Start

```bash
mkdir -p ~/commander && cd ~/commander &&
curl -fLO https://github.com/plaxtoris/commander/releases/download/latest/commander &&
chmod +x commander && ./commander
```

Connect accounts, open **Sessions**, choose the accounts and repositories for
this task, then open its shell. Start your agent in that shell. Linux and WSL
on x86_64 are supported. Project containers need Docker Engine or Docker Desktop.

Each session selects its own accounts across companies. A GitHub account and
its repository scope are separate: the same login can serve `plaxtoris/*` in
one session and `ZalaziumGmbH/anox-pro` in another. An empty repository selection
allows no GitHub repository access. Existing profiles remain optional saved
selections. Editing a saved selection never changes running sessions.

```bash
./commander session new release zalazium --repos ZalaziumGmbH/anox-pro
./commander session new mixed --accounts github=plaxtoris,hetzner=dev --repos plaxtoris/*
./commander session shell SESSION_ID
./commander context
./commander exec -- gh api user
./commander exec -- gh pr list --repo ZalaziumGmbH/anox-pro
```

Use `--session ID` before a command outside its shell. For several accounts of
one service use `exec --account SELECTOR -- TOOL ARGS`. Git and container startup
also accept `commander --account USER up PROJECT`. Commands isolate native login
state and preserve exit status. Account refreshes are coordinated without a
global active account switch. Calls sharing mutable credentials may briefly
wait for each other; independent accounts run concurrently.

Selections guide trusted tools; they do not sandbox arbitrary programs or reduce
provider token permissions. Use provider restricted credentials when stronger
boundaries are required. Session selection grants no permission for actions.

## Concurrent repository work

Keep clones in `workspaces/<owner>/<repo>/`. Read each project's README.

```bash
./commander task new zalazium/anox-pro fix-login main
# Work in the printed directory, run checks and commit the task files.
./commander task finish fix-login -- make test
```

Each task gets its own worktree, index and temporary branch. Integration is
serialized per repository. `finish` validates the merged result before pushing,
then verifies the remote target contains the task and removes its temporary
branches, worktree and containers. Failed checks and conflicts preserve the
work. Protected branches need a PR: merge through the repository's checks and
run `finish` again. Prefer a merge commit so Git can prove integration directly.
Interrupted creation or cleanup can be resumed by repeating the same command.
Never bypass required reviews or discard unrelated changes.

## Project containers

```bash
./commander up zalazium/anox-pro
./commander container PROJECT_ID -- make test
```

Build Git into the project image. Commander connects Git on first startup,
including lifecycle commands and manual editor terminals. It supplies the
session's selected GitHub login without mounting the account store. Worktree
containers also mount their Git administrative directory. No agent, agent login
or persistent agent volume belongs in project images.

The runtime connection also offers `commander-make dev` and `commander-make prd`
for product Makefiles. These run the same product target on the host with the
session's tools and stream its output and exit status. Deployment logic stays
in the product and its infrastructure tools. Only trusted projects may connect.
Close finished sessions with `commander session close ID`; task completion is
required first. Docker manages its own images and build caches.

## Transfer and memory

The menu and `export [FILE.zip]` let you select individual accounts, saved
selections and optionally `MEMORY.md`. Any of these can be transferred alone.
Import previews selections and conflicts. Keep or replace each conflicting
account or selection; keep, merge or replace memory. Archives are encrypted,
existing archives remain readable, and running session selections stay intact.
Agent logins are never included.

`commander memory` returns shared preferences and a revision. Update with
`commander memory write REVISION` and the full bullet list on stdin. Concurrent
updates require rereading and merging. Memory grants no account scope or action
permission. Personal preferences survive updates.
