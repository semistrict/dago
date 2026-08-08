package main

import (
	"context"
	"fmt"
	"log"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
)

func main() {
	chat := modeltest.New(model.Profile{}, modeltest.Step{
		Response: model.Response{Message: message.Assistant("The scripted deep agent is ready.")},
	})
	compiled, err := dago.New(dago.Options{Model: chat, DisableSubagents: true, DisableSummary: true})
	if err != nil {
		log.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{
		Messages: []message.Message{message.Human("Introduce yourself.")},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Messages[len(result.Messages)-1].TextContent())
}
