package server

import (
	"github.com/semistrict/dago/examples/shelley/ui"
	"github.com/semistrict/dago/examples/shelley/version"
)

func init() {
	version.RegisterBuildInfoFS(ui.Dist)
}
