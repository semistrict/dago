// Shelley is a mobile-first, multi-conversation coding-agent example built on dago.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	var address, workspace, dataDirectory string
	flag.StringVar(&address, "addr", "127.0.0.1:9000", "HTTP listen address")
	flag.StringVar(&workspace, "workspace", ".", "local workspace root")
	flag.StringVar(&dataDirectory, "data", "", "application data directory")
	flag.Parse()

	if dataDirectory == "" {
		userConfig, err := os.UserConfigDir()
		if err != nil {
			log.Fatal(err)
		}
		dataDirectory = filepath.Join(userConfig, "dago-shelley")
	}
	application, err := newApplication(workspace, dataDirectory)
	if err != nil {
		log.Fatal(err)
	}
	defer application.close()

	server := &http.Server{
		Addr:              address,
		Handler:           application.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	fmt.Printf("Shelley is listening on http://%s\n", address)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
