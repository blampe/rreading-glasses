package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/alecthomas/kong"
	charm "github.com/charmbracelet/log"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/stampede"
)

type cli struct {
	Port   int    `default:"8788"`
	RPM    int    `default:"60" help:"maximum upstream requests per minute"`
	Cookie string `help:"cookie to use for upstream HTTP requests"`

	Verbose bool `help:"increase log verbosity"`

	PostgresHost     string `default:"localhost"`
	PostgresUser     string `default:"postgres"`
	PostgresPassword string `default:""`
	PostgresPort     int    `default:"5432"`
	PostgresDatabase string `default:"rreading-glasses"`

	Proxy string `default:"" help:"HTTP proxy URL to use for upstream requests"`

	Upstream string `required:"" help:"upstream host (e.g. www.example.com)"`
}

// dsn returns the database's DSN based on the provided flags.
func (c *cli) dsn() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s",
		c.PostgresUser,
		c.PostgresPassword,
		c.PostgresHost,
		c.PostgresPort,
		c.PostgresDatabase,
	)
}

func (c *cli) Run() error {
	ctx := context.Background()
	cache, err := newCache(ctx, c.dsn())
	if err != nil {
		return fmt.Errorf("setting up cache: %w", err)
	}

	if c.Verbose {
		_logHandler.SetLevel(charm.DebugLevel)
	}

	upstream, err := newUpstream(c.Upstream, c.Cookie, c.Proxy)
	if err != nil {
		return err
	}

	core := notImplemented{}

	ctrl, err := newController(cache, core, upstream)
	if err != nil {
		return err
	}
	h := newHandler(ctrl)
	mux := newMux(h)

	mux = stampede.Handler(1024, 0)(mux)    // Coalesce requests to the same resource.
	mux = middleware.RequestSize(1024)(mux) // Limit request bodies.
	mux = middleware.RedirectSlashes(mux)   // Normalize paths for caching.
	mux = requestlogger{}.Wrap(mux)         // Log requests.
	mux = middleware.RequestID(mux)         // Include a request ID header.
	mux = middleware.Recoverer(mux)         // Recover from panics.

	// mux = httprate.Limit(5, time.Second)(mux) // TODO: Limit clients to ??? RPS/RPH.

	// TODO: The client doesn't send Accept-Encoding and doesn't handle
	// Content-Encoding responses. This would allow us to send compressed bytes
	// directly from the cache.

	addr := fmt.Sprintf(":%d", c.Port)
	s := &http.Server{
		Handler:  mux,
		Addr:     addr,
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}

	slog.Info("listening on " + addr)
	return s.ListenAndServe()
}

func main() {
	kctx := kong.Parse(&cli{})
	err := kctx.Run()
	if err != nil {
		log(context.Background()).Error("fatal", "err", err)
		os.Exit(1)
	}
}

func init() {
	// Limit our memory to 90% of what's free. This affects cache sizes.
	_, err := memlimit.SetGoMemLimitWithOpts(
		memlimit.WithRatio(0.9),
		memlimit.WithLogger(slog.Default()),
		memlimit.WithProvider(
			memlimit.ApplyFallback(
				memlimit.FromCgroup,
				memlimit.FromSystem,
			),
		),
	)
	if err != nil {
		panic(err)
	}
}
