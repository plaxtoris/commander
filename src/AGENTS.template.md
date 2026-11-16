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
and your shared `MEMORY.md` of lasting user preferences, independent of profiles
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

1. Before service work, run `./commander context` or `./commander context PROFILE`.
   Use only that profile's accounts. If it is missing or empty, ask the user to
   add accounts in Commander. Continue local work that needs no account.
2. Run tools with `./commander exec -- TOOL ARGS`. This supplies the tools and
   login paths. Preserve arguments and exit codes; use native help as needed.
   From project folders, use the absolute path to the Commander executable.
3. Verify identity before acting. Prefer explicit account, subscription or
   context selectors. For GitHub and Microsoft 365, switch accounts only while
   other native commands are idle, then restore the previous account.
   Profiles define scope, not permission to change resources or send messages.
   Docker Hub and Confluence Cloud use built in `dockerhub` and `confluence` CLIs:
   `whoami` verifies identity; `api PATH [-X METHOD] [--input FILE|-]` calls the API.
   Pass `--account` from `context` when several accounts are selected. Run `--help`
   for examples. These CLIs enforce the current profile and verify each request.
   Native Docker and project containers use the profile's sole Docker Hub account.
   For several Hub accounts use `dockerhub docker --account USER -- DOCKER_ARGS`.
   Add or reconnect tokens through Commander Accounts; never read token files.
4. Keep repositories in `workspaces/` and read each project's instructions.
   When creating or cloning repositories, prefer `workspaces/<profile-name>/<repo-name>/`
   where practical, using the relevant Commander profile's name.
   Start trusted projects with `./commander up PROJECT`. Run container commands
   with `./commander exec -- devcontainer exec --workspace-folder /absolute/project COMMAND`.
   Trust specific repositories. Docker uses the host engine and keeps its own
   images and volumes outside this folder.
5. Native credentials belong in `.secrets/`, private and excluded from Git.
   Never read or print them. Use Commander's export and import commands only
   for requested login transfers. Never manage or copy agent logins.

Use `./commander overview` for accounts, `./commander doctor` for tools,
`./commander setup` to repair used tools and `./commander help` for commands.
