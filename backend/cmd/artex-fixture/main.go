package main

// artex-fixture is the explicit process boundary for ARTEX protocol tests and
// the product fixture stack. It serves only the routes verified against the
// local ARTEX source (see internal/artexfixture) and is never linked into the
// production server.
//
// It refuses to start without a token and without a loopback listen address, so
// a fixture cannot accidentally become an unauthenticated public service. State
// is persisted to <data-dir>/artex-state.json after each mutation; a restart
// therefore keeps the goals the previous run created, which is what makes
// "external success, local checkpoint missing" a testable restart state.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"bba.local/taskengine/internal/artexfixture"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Printf("artex-fixture: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("artex-fixture", flag.ContinueOnError)
	listen := fs.String("listen", "", "loopback listen address, e.g. 127.0.0.1:18787 (required)")
	dataDir := fs.String("data-dir", "", "state directory; artex-state.json is read on start and written after each mutation (required)")
	token := fs.String("token", "", "single accepted bearer token (required; falls back to ARTEX_FIXTURE_TOKEN)")
	seed := fs.String("seed", "", "optional seed state file; defaults to <data-dir>/artex-state.json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *listen == "" {
		return errors.New("-listen is required")
	}
	if err := requireLoopback(*listen); err != nil {
		return err
	}
	if *dataDir == "" {
		return errors.New("-data-dir is required")
	}
	authToken := *token
	if authToken == "" {
		authToken = os.Getenv("ARTEX_FIXTURE_TOKEN")
	}
	if authToken == "" {
		return errors.New("-token or ARTEX_FIXTURE_TOKEN is required")
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	statePath := filepath.Join(*dataDir, "artex-state.json")
	loadPath := statePath
	if *seed != "" {
		loadPath = *seed
	}
	state, err := artexfixture.LoadState(loadPath)
	if err != nil {
		return err
	}
	fixture, err := artexfixture.New(artexfixture.Options{Token: authToken, State: state, Path: statePath})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	server := &http.Server{
		Handler:           fixture,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	fmt.Printf("listening %s\n", listener.Addr().String())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

// requireLoopback keeps the fixture off every non-local interface: it has one
// shared bearer token and no rate limit, so binding it to 0.0.0.0 would expose a
// fake ARTEX to the network.
func requireLoopback(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("-listen %q: %w", listen, err)
	}
	if host == "" {
		return fmt.Errorf("-listen %q must name a loopback host, not every interface", listen)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("-listen %q is not a loopback address", listen)
	}
	return nil
}
