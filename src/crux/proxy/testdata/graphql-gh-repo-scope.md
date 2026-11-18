# gh repository-scope document fixtures

`graphql-gh-repo-scope.json` preserves the lookup → mutation document shapes for
`gh issue create/comment/edit/close` and `gh pr create/comment/edit/close/merge`.
These are normalized fixtures, not live credential-bearing traffic captures:
selection sets are reduced to attribution-relevant fields, IDs and content are
synthetic, and JSON whitespace is not significant. The named mutations and input
shapes follow cli/cli's `api/queries_issue.go`, `api/queries_pr.go` and generated
GraphQL operations (reference: cli/cli commit
`acee710547dcf7dfb6490afea77972187e74b819`).

Each fixture records the lookup request, its GitHub response and the subsequent
write request. The proxy integration test replays them on separate connections,
checks that reads and in-scope writes reach upstream unchanged, and checks that
out-of-scope writes never reach upstream. It also replays each ID-only mutation
without the lookup to lock down the explicitly fail-open unresolved-ID policy.
These tests do not execute gh or contact GitHub.
