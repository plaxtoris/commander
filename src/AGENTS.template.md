# Commander

Commander keeps native tools, accounts and projects in this folder.
Run `./commander` for the menu. Start your preferred agent here independently.
Opening the menu automatically updates and restarts Commander when needed, then
installs missing or changed tools. Direct commands such as `exec` do not auto-update.

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
4. Keep repositories in `workspaces/` and read each project's instructions.
   Start trusted projects with `./commander up PROJECT`. Run container commands
   with `./commander exec -- devcontainer exec --workspace-folder /absolute/project COMMAND`.
   Trust specific repositories. Docker uses the host engine and keeps its own
   images and volumes outside this folder.
5. Native credentials belong in `.secrets/`, private and excluded from Git.
   Never read or print them. Use Commander's export and import commands only
   for requested login transfers. Never manage or copy agent logins.

Use `./commander overview` for accounts, `./commander doctor` for tools,
`./commander setup` to finish setup and `./commander help` for commands.
