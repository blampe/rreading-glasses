package main

import (
	"context"
	"encoding/json"
	"errors"
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

// cli contains our command-line flags.
type cli struct {
	Serve server `cmd:"" help:"Run an HTTP server."`

	Bust bust `cmd:"" help:"Bust cache entries."`
}

type server struct {
	pgconfig
	logconfig

	Port     int    `default:"8788" help:"Port to serve traffic on."`
	RPM      int    `default:"60" help:"Maximum upstream requests per minute."`
	Cookie   string `help:"Cookie to use for upstream HTTP requests."`
	Proxy    string `default:"" help:"HTTP proxy URL to use for upstream requests."`
	Upstream string `required:"" help:"Upstream host (e.g. www.example.com)."`
}

type bust struct {
	pgconfig
	logconfig

	AuthorID int64 `arg:"" help:"author ID to cache bust"`
}

type pgconfig struct {
	PostgresHost     string `default:"localhost" help:"Postgres host."`
	PostgresUser     string `default:"postgres" help:"Postgres user."`
	PostgresPassword string `default:"" help:"Postgres password."`
	PostgresPort     int    `default:"5432" help:"Postgres port."`
	PostgresDatabase string `default:"rreading-glasses" help:"Postgres database to use."`
}

// dsn returns the database's DSN based on the provided flags.
func (c *pgconfig) dsn() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s",
		c.PostgresUser,
		c.PostgresPassword,
		c.PostgresHost,
		c.PostgresPort,
		c.PostgresDatabase,
	)
}

type logconfig struct {
	Verbose bool `help:"increase log verbosity"`
}

func (c *logconfig) Run() error {
	if c.Verbose {
		_logHandler.SetLevel(charm.DebugLevel)
	}
	return nil
}

func (s *server) Run() error {
	_ = s.logconfig.Run()

	ctx := context.Background()
	cache, err := newCache(ctx, s.dsn())
	if err != nil {
		return fmt.Errorf("setting up cache: %w", err)
	}

	core := notImplemented{}
	ctrl, err := newController(cache, core)
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

	addr := fmt.Sprintf(":%d", s.Port)
	server := &http.Server{
		Handler:  mux,
		Addr:     addr,
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}

	slog.Info("listening on " + addr)
	return server.ListenAndServe()
}

func (b *bust) Run() error {
	_ = b.logconfig.Run()
	ctx := context.Background()

	cache, err := newCache(ctx, b.dsn())
	if err != nil {
		return err
	}

	a, ok := cache.Get(ctx, authorKey(b.AuthorID))
	if !ok {
		return nil
	}

	var author authorResource
	err = json.Unmarshal(a, &author)
	if err != nil {
		return err
	}

	for _, w := range author.Works {
		for _, b := range w.Books {
			err = errors.Join(err, cache.Delete(ctx, bookKey(b.ForeignID)))
		}
		err = errors.Join(err, cache.Delete(ctx, workKey(w.ForeignID)))
	}
	err = errors.Join(err, cache.Delete(ctx, authorKey(author.ForeignID)))

	return err
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
