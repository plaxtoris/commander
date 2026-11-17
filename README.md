# Commander

One folder for your accounts, tools and repositories, ready for any coding agent.

Sign in once to GitHub, Azure, Microsoft 365, Google Cloud, Hetzner, Docker Hub
and Confluence. Then give each piece of work a **session**: a named selection of
accounts and repositories. Open its shell, start your agent, and it works with
exactly that selection.

```sh
mkdir -p ~/commander && cd ~/commander
curl -fLO https://github.com/plaxtoris/commander/releases/download/latest/commander
chmod +x commander && ./commander
```

```
  commander                                                      2 sessions
  ──────────────────────────────────────────────────────────────────────────

  › client      github alice · confluence alice
                acme/*
    personal    github alice · hetzner home · dockerhub alice
                alice/*
    + new session

  [enter] open  [n]ew  [e]dit  [r]ename  [d]elete  [i]mport  e[x]port  [q]uit
```

- **Many accounts, no switching.** Every command gets only its session's logins.
  No global active account ever changes.
- **Parallel work.** Sessions, task worktrees and project containers run side by side.
- **Portable.** Tools install on demand inside the folder. Move it, or export
  accounts and sessions to an encrypted archive.
- **Agent ready.** Commander writes `AGENTS.md` into its folder. Agents learn the
  rest from `./commander help`.

Linux and WSL on x86_64. Project containers need Docker Engine or Docker Desktop.

## Development

Open this repository in its development container and run `make test` and
`make build`. The source lives in `src/`: the generated agent guidance in
`src/AGENTS.template.md`, the command reference in `src/commander.go` and tool
pins in `src/tools.go`. Use synthetic login fixtures, never real credentials.
CI also tests races and real containers, then publishes a fresh `latest`
release for every build of `main`.
