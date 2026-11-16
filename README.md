# Commander

Commander is a portable workspace that gives your AI agent the tools, accounts
and memory to work across your development stack.

Real work rarely stops at one repository. A feature can touch application code,
cloud infrastructure, containers and business tools. Commander brings those
systems into one working context so you spend less time switching accounts,
repeating instructions and guiding every handoff.

Connect your accounts, choose a profile and describe the outcome. Your agent
uses native tools to coordinate changes across repositories and infrastructure.

**One connected stack.** GitHub, Docker Hub, Microsoft 365, Azure, Google Cloud,
Hetzner and Confluence Cloud. Profiles keep account scope clear.

**An agent that learns how you work.** Commander combines agent instructions
with a small personal memory. Your agent retains lasting preferences, updates
them as you work and tells you what changed.

**Your choice of agent.** Keep the same workspace and preferences when you switch
agents. Tools install when needed. Commander updates itself. Your projects and
memory stay with you.

> “Implement this feature across the services, prepare the infrastructure changes
> and verify that everything works together.”

## Install and run

```bash
mkdir -p ~/commander && cd ~/commander &&
curl -fLO https://github.com/plaxtoris/commander/releases/download/latest/commander &&
chmod +x commander && ./commander
```

Connect your accounts in the menu, then start your preferred AI agent in that folder.

For WSL and Linux on x86_64. Docker Engine or Docker Desktop is needed for project
containers.

## Development

Keep Commander small and independent of any agent. Code and tests live in `src/`;
tool pins live in `src/tools.go`. `src/AGENTS.template.md` generates client
instructions. Preserve personal memory across updates. Memory is English;
replies follow the user's requested language.

Use the VS Code devcontainer. Run `make test` and `make build`. Check setup,
relocation and updates when affected. Test container changes in a disposable
container. Use local authentication fixtures. Never read real credentials or
manage agent logins. Verify identity before service work.

Write short, direct English without em dashes or hyphenated prose. CI publishes
`commander` and `SHA256SUMS` under the single rolling `latest` release. Preserve
repository visibility. Run reset helpers only when requested.
