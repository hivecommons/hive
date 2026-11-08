# Build pipeline

CI is the only verdict. Do not run `go test` or `go build` from an agent pane.

```sh
gh pr checks 1234
```

Use the failing job log to fix, then push again.
