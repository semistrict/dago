package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	address := flag.String("address", "127.0.0.1:0", "loopback listen address")
	failAfter := flag.String("fail-after", "", "owner-only marker that makes later starts fail")
	flag.Parse()
	if *failAfter != "" {
		if _, err := os.Lstat(*failAfter); err == nil {
			fmt.Fprintln(os.Stderr, "fixture restart failed")
			os.Exit(1)
		}
		if err := os.WriteFile(*failAfter, []byte("started\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "write restart marker")
			os.Exit(1)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("ok\n"))
	})
	server := &http.Server{Addr: *address, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "serve restart fixture")
		os.Exit(1)
	}
}
