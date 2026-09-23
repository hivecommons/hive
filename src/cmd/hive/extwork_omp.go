//go:build extwork_omp

package main

import (
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/extwork/omp"
)

// The OMP workbench host adapter (#8361 step 9, #6899) is linked into the
// hive binary only when the extwork_omp build tag is set. Without the tag the
// package is absent from the dependency closure, so no configuration can
// select it; pkg/extwork's TestOMPAdapterNotLinkedUnlessEnabled enforces both
// halves. With the tag, the adapter is registered but still inert until
// runs.external.omp.enabled is true. The tag is independent of extwork_flue.
func init() {
	extwork.DefaultRegistry.Register(omp.Engine, omp.Factory)
}
