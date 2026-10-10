package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime/podman"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/store"
)

// DestroyHostConfig configures DestroyHost.
type DestroyHostConfig struct {
	// HostID is the scheduler identity whose things are destroyed. If it
	// is empty it is derived from RunDir exactly as `sbxd serve` does.
	HostID string
	// RunDir is that scheduler's run dir. When set, DestroyHost refuses
	// to run while a server holds its lock, and removes the per-sandbox
	// directories in it. When empty, no directory is touched.
	RunDir string
	// PodmanSocket is the Libpod API socket; default as for serve.
	PodmanSocket string
	// DatabaseURL, when set, also closes out the host's sandbox rows
	// (reason host_drain), so that tenants' quota slots are freed and
	// nothing is left for a later start to reconcile.
	DatabaseURL string
	Logger      *slog.Logger
}

// DestroyHostReport says what DestroyHost removed.
type DestroyHostReport struct {
	HostID     string `json:"host_id"`
	Containers int    `json:"containers"`
	RunDirs    int    `json:"run_dirs"`
	Rows       int    `json:"rows"`
}

// DestroyHost removes everything a scheduler identity owns without
// starting the scheduler: every container labelled sbx.managed with that
// sbx.host_id, the sandbox run dirs, and (with a database) the host's
// live sandbox rows. It is the offline twin of the "destroy" shutdown
// policy, for a host that is being decommissioned or whose server cannot
// be started any more. Containers of any other host id, and containers
// without one, are never touched.
func DestroyHost(ctx context.Context, cfg DestroyHostConfig) (DestroyHostReport, error) {
	var rep DestroyHostReport
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.RunDir != "" {
		abs, err := filepath.Abs(cfg.RunDir)
		if err != nil {
			return rep, fmt.Errorf("run dir: %w", err)
		}
		cfg.RunDir = abs
	}
	if cfg.HostID == "" {
		if cfg.RunDir == "" {
			return rep, errors.New("a host id is required (--host-id), or the run dir it is derived from (--run-dir)")
		}
		id, err := DefaultHostID(cfg.RunDir)
		if err != nil {
			return rep, err
		}
		cfg.HostID = id
	}
	if err := runtime.ValidHostID(cfg.HostID); err != nil {
		return rep, fmt.Errorf("--host-id: %w", err)
	}
	rep.HostID = cfg.HostID

	// A running server would re-create what is destroyed here, and its
	// sessions would be cut from under it.
	if cfg.RunDir != "" {
		if _, err := os.Stat(cfg.RunDir); err == nil {
			unlock, err := lockRunDir(cfg.RunDir)
			if err != nil {
				return rep, fmt.Errorf("%w; stop that server first (or stop it with --shutdown-policy destroy)", err)
			}
			defer unlock()
		}
	}

	backend, err := podman.New(podman.Config{SocketPath: cfg.PodmanSocket})
	if err != nil {
		return rep, err
	}
	if _, err := backend.Ping(ctx); err != nil {
		return rep, fmt.Errorf("podman socket %s: %w", backend.Config().SocketPath, err)
	}

	// Rows first: from the moment a row is RECYCLING its access token is
	// dead and it no longer counts against its tenant's quota.
	var (
		st     *store.Store
		fenced []core.Sandbox
	)
	if cfg.DatabaseURL != "" {
		if st, err = store.Open(ctx, cfg.DatabaseURL); err != nil {
			return rep, err
		}
		defer st.Close()
		rows, err := st.ListActiveByHost(ctx, cfg.HostID)
		if err != nil {
			return rep, fmt.Errorf("list sandbox rows: %w", err)
		}
		for _, row := range rows {
			if row.State != core.StateRecycling {
				reason := core.ReasonHostDrain
				if row.State == core.StateFailed || row.State == core.StateUnknown {
					reason = "" // keep the reason that explains the failure
				}
				rec, err := st.Transition(ctx, store.TransitionParams{ID: row.ID, Generation: row.Generation, From: row.State,
					To: core.StateRecycling, Reason: reason, Data: map[string]any{"cause": "admin_host_destroy"}})
				if err != nil {
					return rep, fmt.Errorf("fence sandbox %s: %w", row.ID, err)
				}
				row = rec
			}
			fenced = append(fenced, row)
		}
	}

	instances, err := backend.List(ctx)
	if err != nil {
		return rep, fmt.Errorf("list containers: %w", err)
	}
	for _, inst := range instances {
		if inst.HostID != cfg.HostID {
			continue
		}
		if err := backend.Destroy(ctx, inst.ID); err != nil {
			return rep, fmt.Errorf("destroy container %s (sandbox %s): %w", inst.ID, inst.SandboxID, err)
		}
		rep.Containers++
		log.Info("destroyed container", "instance", inst.ID, "sandbox", inst.SandboxID)
	}
	if left, err := backend.List(ctx); err != nil {
		return rep, fmt.Errorf("list containers: %w", err)
	} else {
		for _, inst := range left {
			if inst.HostID == cfg.HostID {
				return rep, fmt.Errorf("container %s of host %s is still there", inst.ID, cfg.HostID)
			}
		}
	}

	if cfg.RunDir != "" {
		entries, err := os.ReadDir(cfg.RunDir)
		if err != nil && !os.IsNotExist(err) {
			return rep, fmt.Errorf("read run dir: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "sb-") {
				continue
			}
			if err := os.RemoveAll(filepath.Join(cfg.RunDir, e.Name())); err != nil {
				return rep, fmt.Errorf("remove run dir: %w", err)
			}
			rep.RunDirs++
		}
	}

	for _, row := range fenced {
		if _, err := st.Transition(ctx, store.TransitionParams{ID: row.ID, Generation: row.Generation,
			From: core.StateRecycling, To: core.StateDestroyed}); err != nil && !errors.Is(err, store.ErrInvalidTransition) {
			return rep, fmt.Errorf("mark sandbox %s destroyed: %w", row.ID, err)
		}
		rep.Rows++
	}
	return rep, nil
}
