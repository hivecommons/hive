//go:build extwork_vibekanban

package main

import (
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/extwork/vibekanban"
)

func init() {
	extwork.DefaultRegistry.Register(vibekanban.Engine, vibekanban.Factory)
}
