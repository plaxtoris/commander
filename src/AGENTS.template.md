# Commander

Commander keeps native tools, accounts and projects in this folder.
Run `./commander` for the menu. Start your preferred agent here independently.
Opening the menu updates and restarts Commander when needed, then repairs or
updates only tools already used. Tools install on demand when selecting a service,
checking imported accounts, running `exec`, or starting a project container.
Direct commands do not update the Commander executable. Unused tools are normal.
Commander regenerates this file on menu startup, setup and update.
Reply in the language of the current user prompt unless the user requests a
different output language. The language of these instructions or memory does
not determine the reply language.

At the start of each session, run `./commander memory`. It returns a revision
and your shared `MEMORY.md` of lasting user preferences, independent of sessions
and projects. Apply only relevant preferences; current user instructions take
precedence. Memory never grants permission or changes account scope.
When the user states or corrects a lasting preference, update it before your
final response. Keep short plain English bullet points, one statement per line
starting with `* `. Avoid em dashes and hyphenated wording in prose. Do not store
project facts, accounts, secrets, task history, temporary requests or guesses.
On each update, merge duplicates and replace obsolete statements; leave useful
unchanged statements alone. Keep the whole file within 4000 Unicode characters,
including spaces and newlines. If it would exceed the limit, shorten it to about
3500 without losing qualifications or explicit preferences. Save only through
`./commander memory write REVISION`, passing the complete bullet list on stdin
and the revision from the read. On a conflict, read again and merge. Never edit
`MEMORY.md` directly. An empty input clears it. After every saved change, briefly
tell the user what was remembered, corrected or removed. Do not claim a save
that failed. No new lasting preference means no write and no memory notice.

1. Start a session before service work. Use the Sessions menu, or
   `./commander session new NAME PRESET --repos owner/name,owner/*`.
   Run `./commander session shell ID` and start your agent in that shell.
   `./commander context` shows that session's account and repository selection.
   Existing profiles are optional saved selections. Changing one does not change
   running sessions. A GitHub login and its allowed owners or repositories are
   separate choices. Missing accounts can be connected in Commander Accounts.
   Local work without service access can continue with no accounts selected.
2. Run tools through `./commander exec -- TOOL ARGS`. For several accounts of a
   service, use `./commander exec --account SELECTOR -- TOOL ARGS`.
   From project folders use the absolute Commander path. Native commands preserve
   their arguments and exit codes. Each invocation gets its selected login state;
   never switch a global active account. Verify identity before acting.
   Session selections define scope, not permission to change resources or send
   messages. They do not reduce the permissions of a provider token or sandbox
   arbitrary programs. Use only the selected accounts and repositories.
3. Docker Hub and Confluence have built in CLIs: `whoami` verifies identity;
   `api PATH [-X METHOD] [--input FILE|-]` calls the API. Pass their `--account`
   selector when several accounts are selected. Native Docker and project
   containers use the session's sole Docker Hub account; use
   `dockerhub docker --account USER -- DOCKER_ARGS` for several Hub accounts.
4. Keep clones in `workspaces/<owner>/<repo>/` and read each project's README.
   For concurrent changes use `./commander task new PROJECT NAME [BASE]`.
   Work only in the returned worktree. Commit task files, run the project checks,
   and integrate with `./commander task finish NAME -- CHECK ARGS`.
   This checks the merged result, pushes the target and removes merged temporary
   branches, the worktree and its containers. Protected targets require a PR;
   merge it through the repository's checks, then finish the task again.
   Failed checks or conflicts preserve the task. Never discard unfinished work,
   remove unrelated branches, bypass required checks or rewrite shared history.
5. Start trusted projects with `./commander up PROJECT`. Use global
   `--account USER` when selecting a GitHub login among several accounts.
   Commander connects Git at runtime and mounts only a session connection and,
   for worktrees, the Git administrative directory. No agent or login store is
   installed in project images. Use `./commander container ID -- COMMAND ARGS`
   for container commands, or attach an editor. Product `make dev` and `make prd`
   can use the connection to run their same Make targets on the host.
   Close completed sessions with `./commander session close ID`.
6. Credentials belong in `.secrets/`, private and excluded from Git. Never read
   or print them. Use selective export and import only for requested transfers.
   Accounts, saved selections and MEMORY.md can be included independently.
   Never manage or copy agent logins.

Use `./commander overview` for account status, `./commander doctor` for tools,
`./commander setup` to repair used tools and `./commander help` for commands.
