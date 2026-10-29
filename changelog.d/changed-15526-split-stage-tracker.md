<!-- release: none -->
- Internal: split `src/pkg/dashboard/stage_tracker.go` (3 298 lines) into four same-package files — `stage_tracker.go`, `stage_tracker_caption.go`, `stage_tracker_build.go`, `stage_tracker_links.go` — to reduce merge conflicts on that hotspot ([#15526](https://github.com/hivecommons/hive/pull/15526)). No behaviour change; exported API is unchanged.
