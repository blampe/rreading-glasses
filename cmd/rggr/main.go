// Package main runs a metadata server using G——R—— as an upstream.
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

	Port       int    `default:"8788" env:"PORT" help:"Port to serve traffic on."`
	RPM        int    `default:"60" env:"RPM" help:"Maximum upstream requests per minute."`
	Cookie     string `required:"" xor:"cookie" env:"COOKIE" help:"Cookie to use for upstream HTTP requests."`
	CookieFile []byte `required:"" type:"filecontent" xor:"cookie" env:"COOKIE_FILE" help:"File with the Cookie to use for upstream HTTP requests."`
	Proxy      string `default:"" env:"PROXY" help:"HTTP proxy URL to use for upstream requests."`
	Upstream   string `required:"" env:"UPSTREAM" help:"Upstream host (e.g. www.example.com)."`
}

func (s *server) Run() error {
	_ = s.LogConfig.Run()

	ctx := context.Background()
	cache, err := internal.NewCache(ctx, s.DSN())
	if err != nil {
		return fmt.Errorf("setting up cache: %w", err)
	}

	if len(s.CookieFile) > 0 {
		s.Cookie = string(bytes.TrimSpace(s.CookieFile))
	}

	upstream, err := internal.NewUpstream(s.Upstream, s.Cookie, s.Proxy)
	if err != nil {
		return err
	}

	// 3RPS seems to be the limit for all gql traffic, regardless of
	// credentials. Batch size was confirmed empirically, although we still
	// occasionally see failures for smaller batches for some reason.
	// NB: 3RPS *should* be possible here, but I think there's a bad
	// interaction between these requests and the upstream HEAD requests
	// elsewhere. Especially if those result in a 404. That seems to trigger
	// the WAF, which blocks everything for a period of time.
	gql, err := internal.NewGRGQL(ctx, upstream, s.Cookie, time.Second/2.0, 6)
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
		os.Exit(0)
		//slog.Info("waiting for denormalization to finish")
		//ctrl.Shutdown(ctx)
		//slog.Info("shutting down http server")
		//_ = server.Shutdown(ctx)
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
