package main

import (
	"context"
	"errors"
	"flag"
	iofs "io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/openthesis/openthesis/internal/api"
	"github.com/openthesis/openthesis/internal/config"
	"github.com/openthesis/openthesis/web"
)

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	stateDir := fs.String("state-dir", defaultStateDir(), "state directory")
	qemu := fs.String("qemu", defaultQEMU(), "path to qemu binary")
	runsc := fs.String("runsc", defaultRunsc(), "path to runsc binary (gVisor backend)")
	kernel := fs.String("kernel", defaultKernelPath(), "path to guest kernel image")
	initBin := fs.String("init-binary", defaultInitBinary(), "path to openthesis-init binary")
	firecracker := fs.String("firecracker", defaultFirecracker(), "path to firecracker binary (for live notebook sessions)")
	jsonLog := fs.Bool("json", false, "JSON log output")
	fs.Parse(args)

	initLogger(*jsonLog)

	cfg := config.Load()
	if *addr != ":8080" {
		cfg.Addr = *addr
	}

	slog.Info("configuration loaded", "config", cfg.String())

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := os.MkdirAll(*stateDir, 0o750); err != nil {
		slog.Error("failed to create state directory", "path", *stateDir, "err", err)
		return 1
	}

	if distFS, err := iofs.Sub(web.DistFS, "dist"); err == nil {
		api.SetWebFS(distFS)
		slog.Info("web dashboard enabled")
	}

	srv, err := api.NewServer(api.ServerConfig{
		Addr:     cfg.Addr,
		StateDir: *stateDir,
		Runner: api.RunnerConfig{
			StateDir:       *stateDir,
			QEMUBin:        *qemu,
			RunscBin:       *runsc,
			KernelPath:     *kernel,
			InitBin:        *initBin,
			FirecrackerBin: *firecracker,
		},
	})
	if err != nil {
		slog.Error("failed to create server", "err", err)
		return 1
	}

	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("server error", "err", err)
		return 1
	}

	slog.Info("server stopped")
	return 0
}
