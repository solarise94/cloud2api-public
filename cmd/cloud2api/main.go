// Command cloud2api is a multi-cloud storage gateway that fronts an OpenList
// instance. DESIGN.md in the repository root is the authoritative spec.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cloud2api/internal/admin"
	"cloud2api/internal/agentapi"
	"cloud2api/internal/auth"
	"cloud2api/internal/config"
	"cloud2api/internal/gateway"
	"cloud2api/internal/keymgr"
	"cloud2api/internal/openlist"
	"cloud2api/internal/reconcile"
	"cloud2api/internal/storageid"
	"cloud2api/internal/store"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"serve"}
	}
	switch args[0] {
	case "serve":
		if err := serve(args[1:]); err != nil {
			slog.Error(err.Error())
			os.Exit(1)
		}
	case "genkey":
		if err := genkey(); err != nil {
			slog.Error(err.Error())
			os.Exit(1)
		}
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `cloud2api - multi-cloud storage gateway (OpenList front end)

Usage:
  cloud2api serve  [-config <path>]   start the HTTP gateway (default command)
  cloud2api genkey                    print a new API key (plaintext + sha256 hash)

Config file resolution: -config flag, then $CLOUD2API_CONFIG,
/etc/cloud2api/config.yaml, then ./config.yaml.
`)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := *cfgPath
	if path == "" {
		path = os.Getenv("CLOUD2API_CONFIG")
	}
	if path == "" {
		for _, p := range []string{"/etc/cloud2api/config.yaml", "config.yaml"} {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}
	if path == "" {
		return errors.New("no config file found (use -config, $CLOUD2API_CONFIG, /etc/cloud2api/config.yaml or ./config.yaml)")
	}

	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// v0.4: SQLite persistence (UPGRADE §5.1). The v0.3 state.json is
	// imported once and renamed; config.yaml accounts/keys are synced as
	// static rows on every start (config stays their source of truth).
	st, err := store.Open(cfg.Admin.DatabasePath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	if cfg.Admin != nil && cfg.Admin.StateFile != "" {
		if err := store.ImportLegacyState(st, cfg.Admin.StateFile); err != nil {
			return fmt.Errorf("import legacy state: %w", err)
		}
	}
	if err := syncStatic(st, cfg); err != nil {
		return fmt.Errorf("sync static config: %w", err)
	}
	// Boot seeding of openlist_public_url (IMPLEMENTATION §部署): a deployment
	// may pass the browser-reachable OpenList address via env. The value is
	// validated first; an invalid one is logged and the stored value kept.
	if base, err := admin.SeedOpenlistPublicURL(st, os.Getenv("C2A_OPENLIST_PUBLIC_URL")); err != nil {
		slog.Error("invalid C2A_OPENLIST_PUBLIC_URL, keeping the stored openlist_public_url", "err", err)
	} else if base != "" {
		slog.Info("openlist_public_url seeded from C2A_OPENLIST_PUBLIC_URL", "url", base)
	}

	keys, err := keymgr.New(st)
	if err != nil {
		return fmt.Errorf("build key manager: %w", err)
	}

	client, err := openlist.New(cfg.Openlist.Endpoint, cfg.Openlist.Token)
	if err != nil {
		return fmt.Errorf("build openlist client: %w", err)
	}
	// v0.3.1: when OpenList credentials are available, an invalidated admin
	// token self-heals (re-login + permanent settings token swap + retry).
	if user, pass := os.Getenv("OPENLIST_USERNAME"), os.Getenv("OPENLIST_PASSWORD"); user != "" && pass != "" {
		client.SetCredentials(user, pass)
		slog.Info("openlist token self-heal enabled")
	}

	registry := gateway.NewAccountRegistry(cfg.Accounts)
	// Dynamic drives (portal-created) come from the store.
	for _, d := range mustListDrives(st) {
		if d.Static {
			continue
		}
		if err := registry.Add(config.Account{
			ID: d.ID, Provider: d.Provider, MountPath: d.MountPath, Enabled: d.Enabled,
		}); err != nil {
			slog.Warn("skipping stored drive", "id", d.ID, "err", err)
		}
	}

	// Phase 1c: the OpenList reconcile loop (§3.7.4) — follows mount renames,
	// tracks disabled/deleted storages, flags replacement storages and
	// auto-adopts static config.yaml accounts by mount path on first boot.
	rec := reconcile.New(st, registry, client, slog.Default())

	// Storage guard (IMPLEMENTATION §存储被删除与替代存储): one store-backed
	// verifier shared by /v1 and the legacy gateway, with the reconcile hook
	// attached so mount renames are followed and replacements are recorded.
	guard := storageid.NewGuard(client,
		func(driveID string) (storageid.Drive, bool) {
			d, err := st.GetDrive(driveID)
			if err != nil {
				return storageid.Drive{}, false
			}
			return storageid.Drive{
				StorageID: d.StorageID, MountPath: d.MountPath,
				Static: d.Static, Enabled: d.Enabled, State: d.State,
			}, true
		},
		func(driveID string) (string, bool) {
			a, ok := registry.Get(driveID)
			return a.MountPath, ok
		}, slog.Default())
	guard.SetReconciler(func(ctx context.Context) { _ = rec.Reconcile(ctx) })

	gw := gateway.New(cfg, keys, client, registry, slog.Default())
	gw.SetStorageGuard(guard)
	gw.SetAudit(func(e store.AuditEntry) {
		if err := st.InsertAudit(e); err != nil {
			slog.Warn("audit insert failed", "err", err)
		}
	})
	gw.SetStorageMissingHook(func() { rec.Trigger("legacy-data-plane") })
	api := agentapi.New(keys, st, client, registry, slog.Default())
	api.SetStorageGuard(guard)
	api.SetReconciler(func(ctx context.Context) { _ = rec.Reconcile(ctx) })

	var handler http.Handler = api.Handler(gw.Handler())
	var portal *admin.Portal
	if cfg.Admin != nil && cfg.Admin.Enabled {
		portal = admin.NewPortal(cfg.Admin, registry, client, st, keys, slog.Default())
		portal.SetWorkspaceHandler(api.WorkspaceHandler())
		portal.SetReconciler(rec)
		// Reauth approvals verify the drive's storage with the same guard the
		// data plane uses (IMPLEMENTATION §agent 的重新登录请求).
		portal.SetStorageGuard(guard)
		// Phase 1d: POST /admin/api/maintenance runs every background loop
		// once (relay retention, trash, pruning).
		portal.SetMaintenanceFunc(api.RunMaintenanceNow)
		handler = portal.Wrap(handler)
	}
	rec.AfterPass = func() {
		if portal != nil {
			portal.InvalidateStorageCache()
		}
		api.InvalidateHealth()
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if portal != nil {
		portal.StartCleanup(ctx)
	}
	api.StartMaintenance(ctx)
	api.StartJobs(ctx)             // resume queued/running jobs after a restart
	api.StartRelayMaintenance(ctx) // Phase 1d: relay retention + chunk purge (§4.7, every 5 min)
	rec.Start(ctx)                 // Phase 1c: initial pass + every 5 minutes (§3.7.4)

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.ListenAndServe()
	}()
	slog.Info("cloud2api listening", "addr", cfg.Listen, "openlist", cfg.Openlist.Endpoint, "db", cfg.Admin.DatabasePath())

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func mustListDrives(st *store.Store) []store.Drive {
	drives, err := st.ListDrives()
	if err != nil {
		slog.Error("cannot list drives from store", "err", err)
		return nil
	}
	return drives
}

// syncStatic mirrors config.yaml accounts and api_keys into the store as
// static rows (read-only in the admin API) and drops static rows that
// disappeared from the config (UPGRADE §4.2 / §8.4).
func syncStatic(st *store.Store, cfg *config.Config) error {
	// --- drives ---
	keptDrives := map[string]bool{}
	for _, a := range cfg.Accounts {
		keptDrives[a.ID] = true
		driver := ""
		if d, ok := admin.DriverFor(a.Provider); ok {
			driver = d
		}
		existing, err := st.GetDrive(a.ID)
		if errors.Is(err, store.ErrNotFound) {
			if err := st.InsertDrive(store.Drive{
				ID:        a.ID,
				Provider:  a.Provider,
				Driver:    driver,
				MountPath: a.MountPath,
				Enabled:   a.Enabled,
				Static:    true,
				CreatedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		existing.Provider = a.Provider
		existing.MountPath = a.MountPath
		existing.Enabled = a.Enabled
		existing.Static = true
		if existing.Driver == "" {
			existing.Driver = driver
		}
		if err := st.UpdateDrive(existing); err != nil {
			return err
		}
	}
	drives, err := st.ListDrives()
	if err != nil {
		return err
	}
	for _, d := range drives {
		if d.Static && !keptDrives[d.ID] {
			if err := st.DeleteDrive(d.ID); err != nil {
				return err
			}
		}
	}

	// --- keys ---
	keptKeys := make([]string, 0, len(cfg.APIKeys))
	for _, k := range cfg.APIKeys {
		keptKeys = append(keptKeys, k.ID)
		perms := expandedPerms(k.Permissions)
		drivesGranted := make([]store.KeyDrive, 0, len(k.Accounts))
		for _, acct := range k.Accounts {
			drivesGranted = append(drivesGranted, store.KeyDrive{Drive: acct, Root: "/", Perms: perms})
		}
		spec := store.APIKey{
			ID:          k.ID,
			Hash:        k.KeyHash,
			Static:      true,
			CreatedAt:   time.Now().UTC(),
			Drives:      drivesGranted,
			GlobalPerms: perms,
			Trash:       store.TrashSettings{Enabled: true, Days: 7},
		}
		if err := st.SyncStaticKey(spec); err != nil {
			return fmt.Errorf("sync static key %q: %w", k.ID, err)
		}
	}
	return st.DeleteStaticKeysNotIn(keptKeys)
}

// expandedPerms maps config permission names onto the v0.4 vocabulary
// (legacy "write" becomes write+delete).
func expandedPerms(in []string) []string {
	out := make([]string, 0, len(in)+1)
	seen := map[string]bool{}
	for _, p := range in {
		for _, mapped := range auth.MapLegacyPermission(p) {
			s := string(mapped)
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func genkey() error {
	plain, _, err := keymgr.GeneratePlaintext()
	if err != nil {
		return err
	}
	fmt.Println(plain)
	fmt.Println(auth.HashKey(plain))
	return nil
}
