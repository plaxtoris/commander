# Commander development

Commander is a portable Go program for WSL and Linux on x86_64. Keep it small,
generic and agent independent. Code and tests live in `src/`; installed agent
instructions come from `src/AGENTS.template.md`. Tool pins live in `src/tools.go`.

Prefer native commands. Preserve arguments, streams and exit codes. Keep runtime
files beside the executable and projects in `workspaces/`. Avoid shell setup,
agent launchers and business specific behavior.

Use only the selected accounts and verify identity before service actions.
Never read or print real credentials or manage agent logins. Test authentication
with local fixtures. Keep `.secrets/` private and ignored by Git.

Run `make test` after changes. Check setup, relocation and updates when affected.
Test container changes in a disposable container. The development container
contains development tools only; the user rebuilds it through VS Code.

Publish one executable named `commander` and `SHA256SUMS` under the single rolling
`latest` release after CI passes. Preserve repository visibility. Run local
repository reset helpers only when the user requests the actual reset.
