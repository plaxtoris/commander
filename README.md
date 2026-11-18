# Commander

One folder for your accounts, tools and repositories, ready for any coding agent.

Sign in once to GitHub, Azure, Microsoft 365, Google Cloud, Hetzner, Docker Hub
and Confluence. Then give each piece of work a **session**: a named selection of
accounts and repositories. Open its shell, start your agent, and it works with
exactly that selection.

```sh
mkdir -p ~/commander && cd ~/commander
curl -fL -o commander https://github.com/plaxtoris/commander/releases/download/latest/commander-$(uname -m)
chmod +x commander && ./commander
```

```
  commander                                                      2 sessions
  ──────────────────────────────────────────────────────────────────────────

  › client      github · confluence
    personal    github · hetzner · dockerhub
    + new session

    github         alice
    confluence     alice@acme.example (acme.atlassian.net)
    repositories   acme/*

  [enter] open  [n]ew  [e]dit  [r]ename  [d]elete  [i]mport  e[x]port  [q]uit
```

- **Many accounts, no switching.** Every command gets only its session's logins.
  No global active account ever changes.
- **Parallel work.** Sessions, task worktrees and project containers run side by side.
  Each terminal keeps its own session; the menu starts at the one you opened last.
- **Portable.** Tools install on demand inside the folder. Move it, or export a
  session with its logins to an encrypted `SESSION.zip`.
- **Agent ready.** Commander writes `AGENTS.md` into its folder. Agents learn the
  rest from `./commander help`.

Linux and WSL on x86_64 and aarch64 (arm64). Project containers need Docker Engine
or Docker Desktop.

## Development

Open this repository in its development container and run `make test` and
`make build`, which writes one binary per machine to `dist/`. The source lives
in `src/`: the generated agent guidance in `src/AGENTS.template.md`, the
command reference in `src/commander.go` and tool pins in `src/tools.go`. Agents learn Commander only from that guidance and
`help`, so keep this README short and inviting for people. Use synthetic login
fixtures, never real credentials.
CI tests races and real containers on x86_64 and aarch64, publishes a fresh
`latest` release for every build of `main` and installs it on both.
