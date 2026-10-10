// Command fakeprovider serves deterministic, non-streaming provider responses
// for local performance measurements. It never calls a real provider.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	defaultAddr     = "127.0.0.1:9000"
	shutdownTimeout = 5 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "fake provider failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("fakeprovider", flag.ContinueOnError)
	flags.SetOutput(output)
	addr := flags.String("addr", defaultAddr, "listen address (local development only)")
	delay := flags.Duration("delay", 0, "fixed response delay, e.g. 50ms")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if strings.TrimSpace(*addr) == "" {
		return errors.New("addr must not be blank")
	}
	if *delay < 0 {
		return errors.New("delay must not be negative")
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	fake := newFakeProvider(*delay)
	srv := &http.Server{
		Handler:           fake.handler,
		ConnState:         fake.connState,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	defer func() {
		srv.Close()
		fmt.Fprintf(output, "fake provider stopped connections=%d\n", fake.connections.Load())
	}()
	fmt.Fprintf(output, "fake provider listening addr=%s delay=%s\n", ln.Addr(), *delay)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		// BaseContext cancels pending delays. Bound draining body reads too.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		if err != nil {
			srv.Close()
		}
		serveErr := <-done
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(err, serveErr)
	}
}
