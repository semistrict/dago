//go:build tinygo

package dago

import (
	"fmt"

	"github.com/semistrict/dago/dagent"
)

func compileInterpreter(Interpreter) (dagent.Middleware, error) {
	return dagent.Middleware{}, fmt.Errorf("JavaScript interpreter is unavailable in TinyGo builds")
}
