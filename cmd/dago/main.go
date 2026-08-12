package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/semistrict/dago/internal/dadev"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 || arguments[0] != "dev" {
		return fmt.Errorf("usage: dago dev [options]")
	}
	flags := flag.NewFlagSet("dago dev", flag.ContinueOnError)
	config := flags.String("config", "dago.json", "path to dago.json")
	flags.StringVar(config, "c", "dago.json", "path to dago.json")
	host := flags.String("host", "localhost", "host to bind")
	port := flags.Int("port", 2024, "port to bind")
	flags.IntVar(port, "p", 2024, "port to bind")
	workers := flags.Int("n-jobs-per-worker", 10, "concurrent run workers")
	noBrowser := flags.Bool("no-browser", false, "do not open Studio")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return dadev.Run(ctx, dadev.Options{
		ConfigPath: *config, Host: *host, Port: *port, Workers: *workers,
		Browser: !*noBrowser, Stdout: os.Stdout, Stderr: os.Stderr,
	})
}
