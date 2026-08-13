package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/semistrict/dago/dacheckpoint"
	checkpointsqlite "github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: sessionseed DATABASE DIRECTORY")
		os.Exit(2)
	}
	if err := seed(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func seed(databasePath, directory string) error {
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		return err
	}
	saver, err := checkpointsqlite.Open(databasePath)
	if err != nil {
		return err
	}
	defer saver.Close()
	agent := dagent.New(modeltest.New(damodel.Profile{}), dagent.Options{Saver: saver})
	for _, session := range []struct {
		id, prompt, answer string
	}{
		{id: "playwright-older", prompt: "Older browser task", answer: "Older browser answer"},
		{id: "playwright-newer", prompt: "Newer browser task", answer: "Newer browser answer"},
	} {
		_, err := agent.UpdateState(context.Background(), dacheckpoint.Config{ThreadID: session.id}, dastate.Values{
			dagent.MessagesKey:           []damessage.Message{damessage.Human(session.prompt), damessage.Assistant(session.answer)},
			"__dacode_working_directory": directory,
		})
		if err != nil {
			return err
		}
	}
	return nil
}
