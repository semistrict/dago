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
	compiled := dago.New(chat, dago.WithFilesystem(dago.Filesystem{}))
	result, err := compiled.Invoke(context.Background(), dagent.Prompt("Introduce yourself."))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Messages[len(result.Messages)-1].TextContent())
}
