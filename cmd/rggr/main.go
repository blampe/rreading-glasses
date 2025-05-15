// Package main runs a metadata server using G——R—— as an upstream.
package main

import (
	"context"
	"encoding/hex"
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

	// These credentials are public and easily obtainable. They are obscured here only to hide them from search results.
	token, _ := hex.DecodeString("6461322d787067736479646b627265676a68707236656a7a716468757779")
	host, _ := hex.DecodeString("68747470733a2f2f6b7862776d716f76366a676733646161616d62373434796375342e61707073796e632d6170692e75732d656173742d312e616d617a6f6e6177732e636f6d2f6772617068716c")

	auth := internal.HeaderTransport{
		Key:          "X-Api-Key",
		Value:        string(token),
		RoundTripper: http.DefaultTransport,
	}
	rate := time.Second

	if s.Cookie != "" {
		// Using an authenticated cookie allows us more RPS.
		rate = time.Second / 3.0
		go func() {
			for {
				key, err := internal.GetGRCreds(ctx, upstream)
				if err != nil {
					internal.Log(ctx).Error("unable to refresh auth", "err", err)
					os.Exit(1)
				}
				auth.Value = key
				time.Sleep(290 * time.Second) // TODO: Use cookie expiration time.
			}
		}()
	}

	gql, err := internal.NewBatchedGraphQLClient(string(host), &http.Client{Transport: internal.ErrorProxyTransport{RoundTripper: auth}}, rate)
	if err != nil {
		return err
	}

	getter, err := internal.NewGRGetter(cache, gql, upstream)
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
		slog.Info("waiting for denormalization to finish")
		ctrl.Shutdown(ctx)
		slog.Info("shutting down http server")
		_ = server.Shutdown(ctx)
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
