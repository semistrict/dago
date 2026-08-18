package dagent

import (
	"testing"
	"time"

	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

func TestStaticExecutionLimitsRejectNegativeValues(t *testing.T) {
	tests := map[string]func(){
		"agent recursion": func() {
			New(modeltest.New(damodel.Profile{}), Options{RecursionLimit: -1})
		},
		"tool retry attempts": func() { ToolRetry(ToolRetryOptions{Attempts: -1}) },
		"tool retry backoff":  func() { ToolRetry(ToolRetryOptions{Backoff: -time.Second}) },
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("negative static limit did not panic")
				}
			}()
			run()
		})
	}
}
