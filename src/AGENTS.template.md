# Commander

Commander manages the tools, accounts and projects for this installation.
Use the absolute Commander path in COMMANDER_HOME from session workspaces.
Reply in the language of the current request unless the user asks otherwise.

## Working style

Complete clear tasks through implementation and verification. Make routine,
reversible decisions yourself. Ask when the user's choice changes the goal,
when scope or authority is unclear, or before irreversible actions.

Reuse existing code and keep one source of truth. Group related behavior in
a small, clear file structure. Add a dependency, file or abstraction only
when it earns its place. Remove obsolete code within the scope of the task.
Measure slow paths and avoid repeated work. Check common workflows, failure
paths and concurrency where shared state can change.

Make the next action easy to find. Use short labels, show status beside the
item it concerns and let users recover where the problem occurs. Test the
interface through its normal entry point, including small terminals when relevant.

## Memory

At the start of each conversation run `./commander memory`. It prints the active
session, a revision and `MEMORY.md`, the user's lasting preferences across
sessions and projects. Begin your first reply with one short line naming that
session, or saying that none is active. Apply the relevant preferences; current
instructions take precedence. Memory never grants permission or changes account
scope.

When the user states or corrects a lasting preference, save it before your final
response: pipe the complete updated list to `./commander memory write REVISION`.
Write short plain English statements, one per line starting with `* `, without
em dashes or hyphenated wording. Merge duplicates, replace obsolete statements
and keep useful ones unchanged. Never store project facts, accounts, secrets,
task history, temporary requests or guesses. The file holds at most 4000
characters; when it would exceed that, shorten it to about 3500 without losing
qualifications or explicit preferences. If the revision changed, read again and
merge. Never edit `MEMORY.md` directly. Then tell the user briefly what was
remembered, corrected or removed, and never claim a save that failed.

## Sessions and accounts

1. Service work happens in a session: a named, lasting selection of accounts and
   GitHub repositories. The user usually starts you in a session shell. Outside
   one, pick the fitting session from `./commander session list` and put
   `--session NAME` right after `./commander`. `./commander context` shows the
   selection and the account selectors. If no session fits, create one with
   `./commander session new NAME --accounts service=user,... --repos
   owner/name,owner/*`. Never edit, rename or delete existing sessions unless
   the user asks; when one lacks an account or repository, tell the user. The
   user connects missing logins in the menu. Local work without service access
   needs no accounts.
2. Run tools as `./commander exec -- TOOL ARGS`, adding `--account SELECTOR`
   after `exec` when more than one login matches. From project
   folders use the absolute Commander path. A saved GitHub login for a repo
   or organization is selected automatically. Each call gets only its selected
   login; never switch a global active account. Verify identity before acting.
   A selection defines scope, not permission to change resources or send
   messages, and it neither sandboxes programs nor reduces token permissions.
3. Docker Hub and Confluence have built in clients, used through `exec`:
   `./commander exec -- dockerhub whoami [--account USER]` verifies identity and
   `./commander exec -- confluence api PATH [-X METHOD] [--input FILE|-]` calls
   the API. Native Docker and project containers use the session's only Docker
   Hub account; with several use `./commander exec -- dockerhub docker --account
   USER -- DOCKER_ARGS`.

## Projects

4. Clone repositories only into `workspaces/<session>/<repo>/`, where
   `<session>` is the exact name of the active session that
   `./commander context` prints, never the repository owner. Read each
   project's README and keep Commander instructions out of project files. For
   concurrent changes run `./commander task new <session>/<repo> NAME [BASE]`
   and work only in the returned worktree. Commit, run the project checks and
   integrate with `./commander task finish NAME -- CHECK ARGS`, where CHECK
   ARGS is the project's check command from its README. It checks the merged
   result, pushes and removes the temporary branches, worktree and containers.
   Protected targets need a PR: merge it through the repository checks, then
   finish again. Failed checks or conflicts keep the task. Never discard
   unfinished work, remove unrelated branches, bypass required checks or rewrite
   shared history.
5. Start trusted projects with `./commander up <session>/<repo>`. It prints the
   container ID for `./commander container ID -- COMMAND ARGS`; you can also
   attach an editor. If the repository has no assigned GitHub login and
   the session has several, put `--account USER` right after `./commander`
   for `up` and `task`. Git
   connects at runtime, so keep project images minimal and free of agents,
   agent state and logins. Product `make dev` and `make prd` can run their
   Make targets on the host through that connection.
6. Credentials live in `.secrets/`. Never read, print or copy them, and never
   manage agent logins. Export and import only when the user asks.

Check account status with `./commander overview`, tools with
`./commander doctor`, and repair them with `./commander setup`.
