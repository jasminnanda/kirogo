// Command kirogo runs a single-user proxy that exposes Kiro (Amazon Q
// Developer / CodeWhisperer) models through OpenAI- and Anthropic-compatible
// HTTP APIs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"kirogo/internal/api"
	"kirogo/internal/auth"
	"kirogo/internal/catalog"
	"kirogo/internal/config"
	"kirogo/internal/kiro"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run loads configuration, starts the HTTP server and blocks until a shutdown
// signal arrives.
func run(args []string) error {
	cfg, err := config.Load(args)
	switch {
	case errors.Is(err, config.ErrVersionRequested):
		fmt.Println("kirogo " + config.Version)
		return nil
	case errors.Is(err, config.ErrHelpRequested):
		fmt.Println(usage)
		return nil
	case err != nil:
		return err
	}

	setupLogging(cfg.LogLevel)

	for _, n := range cfg.Notices {
		slog.Info(n)
	}
	for _, wr := range cfg.Warnings {
		slog.Warn(wr)
	}

	authManager, err := auth.New(auth.Options{
		CredsFile:         cfg.CredsFile,
		RefreshToken:      cfg.RefreshToken,
		CLIDBFile:         cfg.CLIDBFile,
		ProfileARN:        cfg.ProfileARN,
		SSORegion:         cfg.SSORegion,
		APIRegionOverride: cfg.APIRegion,
		KiroVersion:       cfg.KiroVersion,
		SQLiteLoader:      sqliteLoader,
	})
	if err != nil {
		return err
	}
	if authManager.ProfileARN() == "" {
		slog.Warn("no profile ARN is available. runtime.kiro.dev normally requires one; set PROFILE_ARN if requests are rejected.")
	}

	kiroClient := kiro.NewClient(kiro.Options{
		Auth:              authManager,
		AgentMode:         cfg.AgentMode,
		StreamReadTimeout: cfg.StreamingReadTimeout,
	})

	models := catalog.New(catalog.Options{
		Fetcher: kiroClient,
		TTL:     cfg.ModelRefreshTTL,
	})

	// The catalog is required: without it there is no way to know which models
	// exist, what their context windows are, or which effort levels they accept.
	// There is deliberately no embedded fallback list, because a stale hardcoded
	// list is what stops a proxy from ever seeing new models.
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelStartup()
	if err := models.Refresh(startupCtx); err != nil {
		return fmt.Errorf("could not load the model catalog from %s: %w\n\n"+
			"kirogo needs the catalog to start. Things worth checking:\n"+
			"  - your credentials are valid: sign in to Kiro IDE again\n"+
			"  - the region is right: currently %s (override with KIRO_API_REGION)\n"+
			"  - the profile ARN is right: %s (override with PROFILE_ARN)\n"+
			"  - your network allows HTTPS to that host, using HTTPS_PROXY if needed",
			authManager.ControlPlaneHost(), err, authManager.APIRegion(), orNone(authManager.ProfileARN()))
	}

	if cfg.DumpModels {
		dumpModels(models)
		return nil
	}

	srv := api.NewServer(api.Deps{
		Config:  cfg,
		Catalog: models,
		Kiro:    kiroClient,
		Auth:    authManager,
	})

	httpServer := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		// No WriteTimeout: streaming responses legitimately stay open for
		// minutes. Inter-chunk limits are enforced against the upstream
		// connection instead.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("kirogo listening", "addr", cfg.Addr(), "version", config.Version, "models", models.Len())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("could not start HTTP server on %s: %w", cfg.Addr(), err)
		}
		return nil
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining connections")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown did not complete cleanly: %w", err)
	}
	slog.Info("kirogo stopped")
	return nil
}

// dumpModels prints the live catalog as a table.
//
// This is how a user discovers the machine ids behind the labels their Kiro plan
// shows, along with each model's context window, credit multiplier and reasoning
// effort levels. Those ids are supplied by the server and are not guessable.
func dumpModels(models *catalog.Catalog) {
	list := models.Models()
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })

	fmt.Printf("%d models available", len(list))
	if def := models.DefaultModel(); def != "" {
		fmt.Printf(", default %s", def)
	}
	fmt.Print("\n\n")

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "MODEL ID\tNAME\tCONTEXT\tMAX OUT\tRATE\tEFFORT LEVELS\tDEFAULT")
	for _, m := range list {
		rate := "-"
		if m.RateMultiplier > 0 {
			unit := m.RateUnit
			if unit == "" {
				unit = "unit"
			}
			rate = fmt.Sprintf("%gx %s", m.RateMultiplier, unit)
		}
		levels := "-"
		if l := m.EffortLevels(); len(l) > 0 {
			levels = strings.Join(l, ",")
		}
		maxOut := "-"
		if m.MaxOutputTokens > 0 {
			maxOut = fmt.Sprintf("%d", m.MaxOutputTokens)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			m.ID, m.Name, m.MaxInputTokens, maxOut, rate, levels, orDash(m.DefaultEffortLevel()))
	}
	if err := w.Flush(); err != nil {
		slog.Error("could not write the model table", "error", err)
	}

	fmt.Print("\nTo pin a reasoning effort, append it to the model name, for example ")
	for _, m := range list {
		if len(m.EffortLevels()) > 0 {
			fmt.Printf("%s:%s", m.ID, m.EffortLevels()[len(m.EffortLevels())-1])
			break
		}
	}
	fmt.Println(".")
}

// orDash renders an empty string as a dash for table output.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// orNone renders an empty string as "(none)" for error messages.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// setupLogging installs a text handler on stderr at the configured level.
func setupLogging(level slog.Level) {
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}

const usage = `kirogo - Kiro proxy exposing OpenAI- and Anthropic-compatible APIs.

Usage:
  kirogo [flags]

Flags:
  -host string    listen address (default 127.0.0.1, or SERVER_HOST)
  -port int       listen port (default 8000, or SERVER_PORT)
  -dump-models    print the live model catalog and exit
  -version        print version and exit

Configuration is read from flags, the process environment and ./.env,
in that order of precedence. See README.md for the full variable list.`

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.
