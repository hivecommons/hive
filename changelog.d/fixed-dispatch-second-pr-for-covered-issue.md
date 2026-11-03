- Dispatch never opens a second PR for an issue that already has one
  ([#15469](https://github.com/hivecommons/hive/issues/15469)): an open PR's
  webhook that covers an issue now preempts a contributor task already in
  flight on it (the author's own PR excepted); the duplicate-fix sweep tells
  a registered relay contributor's PR from a person's, so its one superseded
  notice passes the write-back gate instead of being suppressed; and a PR
  the merge train holds as `duplicate-of#<winner>` is parked with its author
  (072/071/070, "Duplicate of #winner") without nudge or adoption, never
  "Approved; waiting its turn".
