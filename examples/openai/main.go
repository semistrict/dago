package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/providers/openai"
)

func main() {
	key := os.Getenv("OPENAI_API_KEY")
	modelName := os.Getenv("OPENAI_MODEL")
	if modelName == "" {
		modelName = "gpt-5"
	}
	chat, err := openai.NewAPIKey(key, openai.Options{Model: modelName, ContextWindow: 128_000})
	if err != nil {
		log.Fatal(err)
	}
	workspace, err := backend.NewFilesystem(backend.FilesystemOptions{Root: "."})
	if err != nil {
		log.Fatal(err)
	}
	compiled, err := dago.New(dago.Options{Model: chat, Backend: workspace})
	if err != nil {
		log.Fatal(err)
	}
	stream := compiled.Stream(context.Background(), agent.Input{
		Messages: []message.Message{message.Human("Summarize this workspace.")},
	}, 32)
	defer stream.Close()
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		if event.Mode == agent.EventToken && event.Chunk != nil {
			fmt.Print(event.Chunk.MessageDelta.TextContent())
		}
	}
	fmt.Println()
}
