# Dashboard history reads

`GET /api/history`, `/api/cost/history`, and `/api/knowledge/fact-history`
return their existing JSON arrays with at most **512 points** by default.
Sampling selects whole original rows evenly across the available history,
retaining the first and last points. `max_points=1` selects the newest point.
Stored histories, persistence and server-side accounting are unchanged.

Optional query parameters:

- `since`: exclusive Unix-millisecond timestamp; filtering happens before sampling.
- `max_points`: integer from 1 to 10000. Invalid values or invalid/negative
  timestamps return 400. A full-resolution consumer must request a bound large
  enough for its retained history; sampled chart points are not an accounting
  ledger or a lossless incremental feed.

`X-Hive-History-Total` is the number of matching points before sampling;
`X-Hive-History-Returned` is the response count. The cost and fact aliases under
`/api/timeseries?series=cost|fact` use the same behavior.

The dashboard's cost calculations require every step (including counter resets),
so its first request uses `max_points=8640`, the cost ring's capacity. Subsequent
polls add `since=<last t>` and append only new rows, retaining at most 8640. This
avoids downloading the multi-megabyte ring every minute without deriving spend
from sampled counters. Fact and governor charts use the bounded default.

`GET /api/launch-cost` accepts the same parameters plus the existing `ref` filter.
`since` selects launches by `launched_at` and refs by `last_launched_at`; it is
**not an update cursor** for spend accruing on an older launch. Each detail list
is capped independently: newest launches and highest-spend refs, preserving the
existing ordering. `by_launch_total`, `by_ref_total` and `truncated` disclose
omissions after filtering. Aggregate totals and unattributed/unsupported buckets
always describe the entire accounting window, even when details are filtered.

`GET /api/launch-cost/history` returns `{"daily_rollup": [...]}`, ascending by
UTC `day`. Each day lists `by_ref[]` with `ref`, `kind`, `launches` and `share_usd`
(each launch's spend split evenly across the refs it named). `launches` counts
every delivered kick that named the ref that day, including kicks that received
no usage, matching `/api/launch-cost` `by_ref[].launches`. It is folded from
the launch-cost snapshot on every collection and persisted with it, so it
outlives the `/api/launch-cost` accounting window. Each launch is tracked
individually while the join can still see it (its share follows usage that
keeps landing on it) and is added to its day's settled totals exactly once when
it leaves the window or the launch ring, so a day sums every launch even when
the ring turns over in under 24h. It has no sampling parameters.

All four endpoints, `/api/status` and `/api/status/summary` emit weak ETags with
`Cache-Control: private, no-cache`. Send `If-None-Match` to receive an empty 304
when the selected representation is unchanged. Status field projections retain
their own validators; status gzip and identity responses both vary on
`Accept-Encoding`. Status is a current snapshot, so it is not sampled and does
not accept a history cutoff. SSE continues to deliver live status snapshots.

Launch-cost collection time is informational and excluded from its validator:
recollecting unchanged data does not force a transfer. `collected_at` remains in
200 bodies, and `X-Hive-Collected-At` reports the latest collection time on both
200 and 304 responses. All other payload changes invalidate the response.
