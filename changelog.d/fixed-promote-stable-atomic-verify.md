- Stable promotion now re-verifies every candidate image digest before
  retagging any of them, so a candidate superseded mid-promotion aborts the
  whole run instead of leaving `stable` partially promoted across
  hive/hive-contributor/hive-hub (`src/scripts/promote-stable.sh`).
