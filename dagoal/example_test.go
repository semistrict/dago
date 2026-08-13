package dagoal_test

import (
	"context"
	"fmt"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel/modeltest"
)

func ExampleMiddleware() {
	agent := dagent.New(modeltest.NewPredictable(modeltest.PredictableOptions{}), dagent.Options{
		Middleware: []dagent.Middleware{dagoal.Middleware(dagoal.Options{})},
	})

	result, err := agent.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{
		damessage.Human(`tool: create_goal {"objective":"Finish the release checklist"}`),
	}})
	if err != nil {
		panic(err)
	}
	goal, _ := dagoal.FromState(result.State)

	fmt.Printf("%s: %s\n", goal.Status, goal.Objective)

	// Output:
	// active: Finish the release checklist
}
