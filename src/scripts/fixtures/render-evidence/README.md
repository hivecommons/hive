Synthetic API responses for the UI render-evidence job
(`src/scripts/dashboard-render-evidence.mjs`, #18915).

A request to `/api/<path>` is answered from `api/<path>.json` here, and any
route without a file gets a 404, so a page shows its empty or error state for
data this directory does not provide. Everything is invented: names, hashes and
counts are placeholders and say nothing about any real hive. Keep it that way:
no real organisation, cluster, customer or user names, and no credentials.

To make a UI change show real-looking data in its evidence, add or extend a
file here that mirrors the route's response shape.
