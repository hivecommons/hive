package config

import "github.com/hivecommons/hive/pkg/stagelabels"

// Register from the consumer side: stagelabels must not depend on config
// (which imports claims), so claims can render labels from the same table.
func init() { RegisterStageSlugs(stagelabels.Slugs()) }
