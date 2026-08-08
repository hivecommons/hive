# Docs site publishing

Hive publishes this documentation from the repository source of truth under
`v2/docs/` using MkDocs Material. The root `mkdocs.yml` keeps the standard
`mkdocs build` entry point at the repository root while pointing `docs_dir` at
`v2/docs/` so the versioned docs layout does not move.

On pull requests that touch docs, CI runs a strict MkDocs build. On pushes to
`v4`, the workflow builds the same site and deploys it with GitHub Pages actions
to <https://kubestellar.github.io/hive/>.

## Operator checklist

1. In repository settings, enable GitHub Pages with **GitHub Actions** as the
   source.
2. Confirm the first successful `docs-site` deployment publishes
   <https://kubestellar.github.io/hive/>.
3. Optional custom domain: configure `docs.hive.kubestellar.io` in Pages and add
   the required DNS `CNAME` record pointing at `kubestellar.github.io`.
