package main

import (
	"context"
	"fmt"
	"log"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

func main() {
	chat := modeltest.New(damodel.Profile{}, modeltest.Step{
		Response: damodel.Response{Message: damessage.Assistant("The scripted deep agent is ready.")},
	})
	compiled, err := dago.New(dago.Options{Model: chat, DisableSubagents: true, DisableSummary: true})
	if err != nil {
		log.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), dagent.Input{
		Messages: []damessage.Message{damessage.Human("Introduce yourself.")},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Messages[len(result.Messages)-1].TextContent())
}
