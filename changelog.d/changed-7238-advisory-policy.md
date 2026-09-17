- Moved the advisory digest posting policy out of `cmd/hive`'s `main.go` into
  `pkg/advisory`, which already owned `Digest`
  ([#7238](https://github.com/hivecommons/hive/issues/7238) stage 2). Behaviour
  is unchanged. The update-interval throttle that bounds the GitHub round-trip
  (`governor.advisory.update_interval_s`) is now an encapsulated `PostGate`
  value instead of a package-level struct that tests reached into and reset
  field-by-field, removing a cross-test ordering hazard, and the digest
  build/post predicates take an explicit boolean rather than a `*github.Client`
  they only ever nil-checked.
