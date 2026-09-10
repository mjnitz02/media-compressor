package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mjnitz02/media-compressor/internal/config"
	"github.com/mjnitz02/media-compressor/internal/store"
	"github.com/mjnitz02/media-compressor/internal/web"
)

// runServe is the web UI on its own, with nothing running behind it: a
// read-only look at what the database knows. Useful pointed at a copy of the
// database, and useful on the server itself when you want to read the
// decisions without a daemon holding the port.
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to config.yaml")
	listen := fs.String("listen", "", "override server.listen")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.Paths.Database); err != nil {
		return fmt.Errorf("no database at %s yet: run `media-compressor scan -library NAME` first",
			cfg.Paths.Database)
	}
	db, err := store.Open(cfg.Paths.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	addr := cfg.Server.Listen
	if *listen != "" {
		addr = *listen
	}

	// No engine: this process is not running the loop, and the pages say so
	// rather than offering a button that could not do anything.
	srv, err := startWeb(cfg, db, nil, addr)
	if err != nil {
		return err
	}
	fmt.Printf("web UI on http://%s -- read only; nothing is being scanned or encoded here\n", addr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return shutdownWeb(srv)
}

// startWeb binds the listener before returning, so that a port already in use
// is an error the caller can report rather than something that goes wrong in a
// goroutine nobody is reading.
func startWeb(cfg *config.Config, db *store.Store, engine web.Engine, addr string) (*http.Server, error) {
	handler, err := web.New(web.Options{
		Store: db, Config: cfg, Engine: engine, Version: version,
	})
	if err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{
		Handler: handler,
		// Generous, because this serves one person on a LAN and the only cost
		// of a slow read here is a goroutine.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "web UI stopped: %v\n", err)
		}
	}()
	return srv, nil
}

func shutdownWeb(srv *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}
