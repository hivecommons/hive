# ADR-0005: Forge-neutral source control interface

Status: Accepted (retroactive)

## Context

Hive's first production paths were GitHub-shaped, but the v4 package model
needs agents and schedulers to reason about work without baking GitHub names
into every interface. The forge package defines this boundary explicitly: it is
"not hardcoded to GitHub" and exposes neutral repositories, issues, and change
requests while concrete adapters handle GitHub, GitLab, and Gitea/Forgejo
details ([forge package](https://github.com/kubestellar/hive/blob/v4/v2/pkg/forge/forge.go)).

## Decision

Use a small `Forge` interface and forge-neutral data types (`Repo`, `Issue`,
and `ChangeRequest`) for the common read path and safe write path. The interface
covers repo lookup, open issue and change-request enumeration, comments,
labels, and a neutral `SetHold` gate. `NewForge` selects a concrete adapter by
kind, defaulting an empty kind to GitHub for existing deployments.

Keep merge out of the core interface for now. `MergeOptions` and the optional
`Merger` extension document the intended shape, but no adapter is required to
pretend that GitHub merge methods, GitLab merge-request options, and Gitea merge
styles are already neutral.

## Consequences

Most Hive code can depend on the work-item model instead of GitHub-specific API
types, which makes GitLab and Forgejo support additive rather than a fork of the
scheduler. The hold label gives the system one cross-forge merge gate primitive.
The trade-off is an intentionally incomplete abstraction: callers that need a
real merge still use a forge-specific client until Hive has enough semantics to
standardize that operation honestly.
