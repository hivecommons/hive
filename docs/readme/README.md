# README graphics

The root README is hand-written. Its three charts use the October 2026
snapshot linked in [chart-data.json](chart-data.json). The fourth graphic
shows how people, agents, Hive and work tools fit together.

To redraw the graphics with Node.js 18 or newer:

```sh
node docs/readme/render-charts.mjs
```

Commit the SVGs and `manifest.json` with changes to the inputs or renderer.
CI checks the source and image hashes, the chart counts against the saved
benchmark, and the notes in the root README.

When choosing a new snapshot, update the source links, dates, counts and
notes together. Keep the notes next to the charts. Approval is not proof
that code is correct, and activity counts do not measure time saved.

The old benchmark `--readme` option only works on documents with legacy
`outcomes:start` / `outcomes:end` markers. The root README no longer has
those markers; regenerating benchmark reports must not replace its layout.
