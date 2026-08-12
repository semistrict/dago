package daserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// ListenAndServe runs an Agent Server until ctx is canceled. The listener is
// created before this function blocks, so bind errors are returned immediately.
func ListenAndServe(ctx context.Context, address string, options Options) error {
	server, err := New(options)
	if err != nil {
		return err
	}
	defer server.Close()

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			_ = httpServer.Close()
			return err
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
