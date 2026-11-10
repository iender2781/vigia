package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"vigia/internal/contexto/api"
	"vigia/internal/contexto/config"
	"vigia/internal/contexto/discovery"
	"vigia/internal/contexto/engine"
	"vigia/internal/contexto/ontology"
	"vigia/internal/contexto/store"
	"vigia/internal/contexto/web"
	"vigia/internal/platform/metrics"
)

var mBuild = metrics.Default.NewGauge("contexto_build_info", "Versión del binario.", "version", "commit", "role")

func newLogger(cfg *config.Config) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewJSONHandler(os.Stderr, opts)
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h).With("app", "contexto", "version", version)
}

func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	status := fs.Bool("status", false, "solo mostrar la versión del esquema")
	_ = fs.Parse(args)
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	if err := cfg.RequireDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.TenantID)
	if err != nil {
		return err
	}
	defer st.Close()
	if !*status {
		done, err := store.Migrate(ctx, st.DB)
		for _, m := range done {
			fmt.Println("aplicada:", m)
		}
		if err != nil {
			return err
		}
		if err := st.EnsureTenant(ctx, cfg.TenantName); err != nil {
			return err
		}
	}
	cur, latest, err := store.SchemaVersion(ctx, st.DB)
	if err != nil {
		return err
	}
	fmt.Printf("esquema en la versión %d (última disponible: %d)\n", cur, latest)
	if cur != latest {
		return fmt.Errorf("faltan migraciones por aplicar")
	}
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	role := fs.String("role", envOr("CONTEXTO_ROLE", "all"), "roles separados por comas: api, discovery o all")
	once := fs.Bool("once", false, "discovery: ejecutar un solo ciclo y salir (sin liderazgo)")
	_ = fs.Parse(args)
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	if err := cfg.RequireDB(); err != nil {
		return err
	}
	log := newLogger(cfg)
	roles := map[string]bool{}
	for _, r := range strings.Split(*role, ",") {
		switch r = strings.TrimSpace(r); r {
		case "all":
			roles["api"], roles["discovery"] = true, true
		case "api", "discovery":
			roles[r] = true
		case "ingest", "correlator", "enricher", "mcp":
			return fmt.Errorf("el rol %q llega en una fase posterior (ver docs/hoja de ruta)", r)
		default:
			return fmt.Errorf("rol desconocido %q", r)
		}
	}
	mBuild.Set(1, version, commit, *role)

	o, err := ontology.Load(cfg.OntologyDir)
	if err != nil {
		return err
	}
	eng, err := engine.New(o)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.TenantID)
	if err != nil {
		return err
	}
	defer st.Close()
	cur, latest, err := store.SchemaVersion(ctx, st.DB)
	if err != nil || cur != latest {
		return fmt.Errorf("el esquema de la base de datos no está al día (versión %d, se necesita %d): ejecuta 'contexto migrate'", cur, latest)
	}
	if err := st.EnsureTenant(ctx, cfg.TenantName); err != nil {
		return err
	}
	log.Info("arrancando", "roles", *role, "ontology_rules", len(o.Rules), "prometheus", cfg.PrometheusURLs, "auth_mode", cfg.AuthMode)

	if roles["discovery"] {
		if len(cfg.PrometheusURLs) == 0 {
			return errors.New("el rol discovery necesita CONTEXTO_PROMETHEUS_URLS")
		}
		d := discovery.New(cfg, st, eng, log.With("role", "discovery"))
		if *once {
			rep, err := d.RunOnce(ctx)
			log.Info("ciclo terminado", "report", rep)
			return err
		}
	}
	if roles["api"] && cfg.AuthMode == "token" && len(cfg.APITokens) == 0 {
		log.Warn("CONTEXTO_AUTH_MODE=token sin CONTEXTO_API_TOKENS: nadie podrá usar la API")
	}

	a := &api.API{Cfg: cfg, Store: st, O: o, Log: log.With("role", "api"), Web: web.Handler(), Version: version}
	ready := func(ctx context.Context) error { return st.DB.PingContext(ctx) }
	servers := []*http.Server{{Addr: cfg.OpsAddr, Handler: a.OpsHandler(ready), ReadHeaderTimeout: 10 * time.Second}}
	if roles["api"] {
		servers = append(servers, &http.Server{Addr: cfg.HTTPAddr, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second})
	}
	var wg sync.WaitGroup
	errCh := make(chan error, len(servers)+1)
	for _, s := range servers {
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			log.Info("escuchando", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s: %w", s.Addr, err)
			}
		}(s)
	}
	if roles["discovery"] {
		d := discovery.New(cfg, st, eng, log.With("role", "discovery"))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.Loop(ctx); err != nil {
				errCh <- err
			}
		}()
	}
	var runErr error
	select {
	case <-ctx.Done():
		log.Info("parando")
	case runErr = <-errCh:
		log.Error("error fatal", "err", runErr)
		stop()
	}
	shCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shCtx)
	}
	wg.Wait()
	return runErr
}
