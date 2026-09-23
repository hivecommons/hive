//go:build extwork_flue

package main

import (
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/extwork/flue"
)

// The Flue external-execution adapter (#8361) is linked into the hive binary
// only when the extwork_flue build tag is set. Without the tag the package is
// absent from the dependency closure, so no configuration can select it;
// pkg/extwork's TestFlueAdapterNotLinkedUnlessEnabled enforces both halves.
// With the tag, the adapter is registered but still inert until
// runs.external.flue.enabled is true.
func init() {
	extwork.DefaultRegistry.Register(flue.Engine, flue.Factory)
}
