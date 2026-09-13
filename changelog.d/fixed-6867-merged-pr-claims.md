- The duplicate-PR claim scan now also covers recently-merged PRs (#6867): a
  fix that merged without a closing keyword left its issue open while its claim
  vanished from the ledger the moment the PR left the open set, so the settled
  issue went straight back into the contributor offer pool and to agent
  dispatch — downstream measurement put the cost at ~34% of contributor
  sessions ending in "already resolved on main". Merged claims are marked
  `MergedPR`, suppress exactly like their open counterparts (a merged `Refs #N`
  still releases the epic's remainder after the weak-claim window, anchored at
  the merge), never take the red+stale release valve, and age out 72h after
  merge so nothing is stranded forever.
