package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/alecthomas/kong"
	charm "github.com/charmbracelet/log"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/stampede"
	"golang.org/x/time/rate"
)

// cli contains our command-line flags.
type cli struct {
	Serve server `cmd:"" help:"Run an HTTP server."`

	Bust bust `cmd:"" help:"Bust cache entries."`
}

type server struct {
	pgconfig
	logconfig

	Port     int    `default:"8788" env:"PORT" help:"Port to serve traffic on."`
	RPM      int    `default:"60" env:"RPM" help:"Maximum upstream requests per minute."`
	Cookie   string `env:"COOKIE" help:"Cookie to use for upstream HTTP requests."`
	Proxy    string `default:"" env:"PROXY" help:"HTTP proxy URL to use for upstream requests."`
	Upstream string `required:"" env:"UPSTREAM" help:"Upstream host (e.g. www.example.com)."`

	HardcoverAuth string `required:"" env:"HARDCOVER_AUTH" help:"Hardcover Authorization header, e.g. 'Bearer ...'"`
}

type bust struct {
	pgconfig
	logconfig

	AuthorID int64 `arg:"" help:"author ID to cache bust"`
}

type pgconfig struct {
	PostgresHost     string `default:"localhost" env:"POSTGRES_HOST" help:"Postgres host."`
	PostgresUser     string `default:"postgres" env:"POSTGRES_USER" help:"Postgres user."`
	PostgresPassword string `default:"" env:"POSTGRES_PASSWORD" help:"Postgres password."`
	PostgresPort     int    `default:"5432" env:"POSTGRES_PORT" help:"Postgres port."`
	PostgresDatabase string `default:"rreading-glasses" env:"POSTGRES_DATABASE" help:"Postgres database to use."`
}

// dsn returns the database's DSN based on the provided flags.
func (c *pgconfig) dsn() string {
	// Allow unix sockets.
	if filepath.IsAbs(c.PostgresHost) {
		return fmt.Sprintf("postgres://%s:%s@/%s?host=%s",
			c.PostgresUser,
			c.PostgresPassword,
			c.PostgresDatabase,
			c.PostgresHost,
		)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s",
		c.PostgresUser,
		c.PostgresPassword,
		c.PostgresHost,
		c.PostgresPort,
		c.PostgresDatabase,
	)
}

type logconfig struct {
	Verbose bool `env:"VERBOSE" help:"increase log verbosity"`
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

	upstream, err := newUpstream(s.Upstream, s.Cookie, s.Proxy)
	if err != nil {
		return err
	}

	hcTransport := scopedTransport{
		host: "api.hardcover.app",
		RoundTripper: authTransport{
			header: s.HardcoverAuth,
			RoundTripper: throttledTransport{
				Limiter:      rate.NewLimiter(rate.Every(time.Second), 1), // HC is limitted to 1RPS
				RoundTripper: errorProxyTransport{http.DefaultTransport},
			},
		},
	}

	hcClient := &http.Client{Transport: hcTransport}

	gql, err := newGraphqlClient("https://api.hardcover.app/v1/graphql", hcClient)
	if err != nil {
		return err
	}

	getter, err := newHardcoverGetter(cache, gql, upstream)
	if err != nil {
		return err
	}

	ctrl, err := newController(cache, getter)
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

	go func() {
		slog.Info("listening on " + addr)
		_ = server.ListenAndServe()
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

	ctrl.Run(ctx)

	slog.Info("au revoir!")

	return nil
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
