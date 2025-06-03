// Package main runs a metadata server using Hardcover as an upstream.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/alecthomas/kong"
	"github.com/blampe/rreading-glasses/cmd"
	"github.com/blampe/rreading-glasses/internal"
	"github.com/go-chi/chi/v5/middleware"
)

// cli contains our command-line flags.
type cli struct {
	Serve server `cmd:"" help:"Run an HTTP server."`

	Bust cmd.Bust `cmd:"" help:"Bust cache entries."`
}

type server struct {
	cmd.PGConfig
	cmd.LogConfig

	Port     int    `default:"8788" env:"PORT" help:"Port to serve traffic on."`
	RPM      int    `default:"60" env:"RPM" help:"Maximum upstream requests per minute."`
	Cookie   string `env:"COOKIE" help:"Cookie to use for upstream HTTP requests."`
	Proxy    string `default:"" env:"PROXY" help:"HTTP proxy URL to use for upstream requests."`
	Upstream string `required:"" env:"UPSTREAM" help:"Upstream host (e.g. www.example.com)."`

	HardcoverAuth     string `required:"" env:"HARDCOVER_AUTH" xor:"hardcover-auth" help:"Hardcover Authorization header, e.g. 'Bearer ...'"`
	HardcoverAuthFile []byte `required:"" type:"filecontent" xor:"hardcover-auth" env:"HARDCOVER_AUTH_FILE" help:"File containing the Hardcover Authorization header, e.g. 'Bearer ...'"`
}

func (s *server) Run() error {
	_ = s.LogConfig.Run()

	ctx := context.Background()
	cache, err := internal.NewCache(ctx, s.DSN())
	if err != nil {
		return fmt.Errorf("setting up cache: %w", err)
	}

	upstream, err := internal.NewUpstream(s.Upstream, s.Cookie, s.Proxy)
	if err != nil {
		return err
	}

	if len(s.HardcoverAuthFile) > 0 {
		s.HardcoverAuth = string(bytes.TrimSpace(s.HardcoverAuthFile))
	}

	hcTransport := internal.ScopedTransport{
		Host: "api.hardcover.app",
		RoundTripper: &internal.HeaderTransport{
			Key:          "Authorization",
			Value:        s.HardcoverAuth,
			RoundTripper: http.DefaultTransport,
		},
	}

	hcClient := &http.Client{Transport: hcTransport}

	gql, err := internal.NewBatchedGraphQLClient("https://api.hardcover.app/v1/graphql", hcClient, time.Second, 20 /* Not sure about this */)
	if err != nil {
		return err
	}

	getter, err := internal.NewHardcoverGetter(cache, gql, upstream)
	if err != nil {
		return err
	}

	ctrl, err := internal.NewController(cache, getter)
	if err != nil {
		return err
	}
	h := internal.NewHandler(ctrl)
	mux := internal.NewMux(h)

	mux = middleware.RequestSize(1024)(mux)  // Limit request bodies.
	mux = internal.Requestlogger{}.Wrap(mux) // Log requests.
	mux = middleware.RequestID(mux)          // Include a request ID header.
	mux = middleware.Recoverer(mux)          // Recover from panics.

	// TODO: The client doesn't send Accept-Encoding and doesn't handle
	// Content-Encoding responses. This would allow us to send compressed bytes
	// directly from the cache.

	addr := fmt.Sprintf(":%d", s.Port)
	server := &http.Server{
		Handler:  mux,
		Addr:     addr,
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}

	go func() {
		slog.Info("listening on " + addr)
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			internal.Log(ctx).Error(err.Error())
			os.Exit(1)
		}
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt)

	go func() {
		<-shutdown
		slog.Info("shutting down http server")
		_ = server.Shutdown(ctx)
		slog.Info("waiting for denormalization to finish")
		ctrl.Shutdown(ctx)
	}()

	ctrl.Run(ctx, 2*time.Second)

	slog.Info("au revoir!")

	return nil
}

func main() {
	kctx := kong.Parse(&cli{})
	err := kctx.Run()
	if err != nil {
		internal.Log(context.Background()).Error("fatal", "err", err)
		os.Exit(1)
	}
}
