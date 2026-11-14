package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/dkmr27/Rradmon/internal/mqtt"
	"github.com/dkmr27/Rradmon/internal/store"
	"github.com/dkmr27/Rradmon/internal/web"
)

// startHTTP serves the UI on addr. It returns the server (for live
// notifications) and a shutdown function. An empty addr disables the UI.
func startHTTP(addr string, st *store.Store, log *slog.Logger) (*web.Server, func(), error) {
	if addr == "" {
		return nil, func() {}, nil
	}
	ws := web.New(st, log)
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("http address %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		ws.LoopbackOnly = true
	} else {
		log.Warn("web UI is reachable from the network: anyone who can reach it can view it; set an admin password, and use HTTPS beyond a home network", "addr", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{Handler: ws, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("web server stopped", "err", err)
		}
	}()
	shown := addr
	if ws.LoopbackOnly {
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		shown = "localhost:" + port
	}
	fmt.Fprintf(os.Stderr, "Web UI: http://%s/\n", shown)
	return ws, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}, nil
}

// runServe shows the UI for an existing database, without a radio.
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbPath := fs.String("db", "rradmon.db", "database file")
	addr := fs.String("http", "127.0.0.1:8080", "web UI address")
	localObs := fs.String("local", "", "treat this MQTT observer id as the local observer")
	fs.Parse(args)
	if _, err := os.Stat(*dbPath); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := setLocalObserver(st, fs, *localObs); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	_, shutdown, err := startHTTP(*addr, st, log)
	if err != nil {
		return err
	}
	defer shutdown()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
	return nil
}

// setLocalObserver stores -local when it was given (an empty value given
// explicitly clears it); without the flag the stored choice stays.
func setLocalObserver(st *store.Store, fs *flag.FlagSet, id string) error {
	given := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "local" {
			given = true
		}
	})
	if !given {
		return nil
	}
	if id != "" && !mqtt.ValidObserverID(id) {
		return fmt.Errorf("-local: invalid observer id %q", id)
	}
	return st.SetSetting("observer.local", id)
}
