package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/daproviders/openai"
)

func main() {
	key := os.Getenv("OPENAI_API_KEY")
	modelName := os.Getenv("OPENAI_MODEL")
	if modelName == "" {
		modelName = "gpt-5"
	}
	if key == "" {
		log.Fatal("OPENAI_API_KEY is required")
	}
	chat := openai.NewAPIKey(key, modelName, openai.Options{ContextWindow: 128_000})
	workspace, err := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: "."})
	if err != nil {
		log.Fatal(err)
	}
	compiled := dago.New(chat, dago.WithBackend(workspace))
	stream := compiled.Stream(context.Background(), dagent.Prompt("Summarize this workspace."))
	defer stream.Close()
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		if event.Mode == dagent.EventToken && event.Chunk != nil {
			fmt.Print(event.Chunk.MessageDelta.TextContent())
		}
	}
	fmt.Println()
}
