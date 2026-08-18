package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "missing draft path")
		os.Exit(2)
	}
	path := os.Args[len(os.Args)-1]
	content, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	edited := "edited by fixture\n"
	if len(content) > 0 {
		edited = "edited: " + string(content) + "\n"
	}
	if strings.TrimSpace(string(content)) == "cancel edit" {
		edited = " \n\t\n"
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
