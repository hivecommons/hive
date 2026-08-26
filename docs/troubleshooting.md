# Troubleshooting

> **Retired.** This page documented the original supervisor/tmux/systemd
> deployment (`bin/supervisor.sh`, `systemctl`, `/etc/hive/agent.env`), which is
> no longer how Hive runs. None of it applies to the current containerized Go
> deployment.
>
> **Current troubleshooting lives at
> [`src/docs/troubleshooting.md`](../src/docs/troubleshooting.md)** — container
> logs, config validation, agent sessions, dashboard auth, and GitHub
> credential checks.
>
> See the [`src/docs/README.md`](../src/docs/README.md) index for the full
> documentation set.

The v1 content that used to live here was removed because it was full-text
searchable and surfaced ahead of the current guide, sending operators to
`systemctl` steps that no longer exist. Recover it from git history if you are
maintaining a v1 deployment:

```sh
git log --all --oneline -- docs/troubleshooting.md
git show <commit>:docs/troubleshooting.md
```
