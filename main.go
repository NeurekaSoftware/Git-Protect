// Command git-backup mirrors git repositories (with optional forge metadata)
// to S3-compatible object storage on a cron schedule.
//
// It is a long-running daemon: it loads settings.yaml, hot-reloads it, and
// loops on the configured schedule until interrupted.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/neurekadev/git-backup/internal/backup"
	"github.com/neurekadev/git-backup/internal/buildinfo"
	"github.com/neurekadev/git-backup/internal/config"
	"github.com/neurekadev/git-backup/internal/forge"
	"github.com/neurekadev/git-backup/internal/git"
	"github.com/neurekadev/git-backup/internal/health"
	"github.com/neurekadev/git-backup/internal/logging"
	"github.com/neurekadev/git-backup/internal/privilege"
	"github.com/neurekadev/git-backup/internal/scheduler"
	"github.com/neurekadev/git-backup/internal/store"
)

const (
	containerConfigPath = "/app/config"
	containerDataPath   = "/app/data"
)

func main() {
	os.Exit(run())
}

func run() int {
	logging.Install()

	buildInfo := buildinfo.LoadFromEnvironment()
	slog.Info("Git Backup started.", "version", buildInfo.Version, "commit", buildInfo.Commit)

	settingsPath := resolveSettingsPath(os.Args[1:])
	slog.Info("Using settings file.", "settingsPath", settingsPath)

	loader := config.NewLoader()
	settings, loadErrors := loader.Load(settingsPath)
	if len(loadErrors) > 0 {
		slog.Error("Failed to load settings file.", "settingsPath", settingsPath)
		for _, loadError := range loadErrors {
			slog.Error("Settings validation error.", "error", loadError)
		}
		return 1
	}

	if level, ok := logging.ParseLevel(settings.Logging.LogLevel); ok {
		logging.SetLevel(level)
	}
	slog.Info("Active log level set.", "logLevel", settings.Logging.LogLevel)

	// The working root must exist and be owned by the runtime identity before
	// the listener, watcher, or scheduler start any work.
	workingRoot, err := resolveWorkingRoot()
	if err != nil {
		slog.Error("Failed to create the working directory.", "error", err.Error())
		return 1
	}
	if err := applyRuntimeIdentity(workingRoot); err != nil {
		slog.Error("Failed to apply the runtime identity.", "error", err.Error())
		return 1
	}
	slog.Info("Working directory ready.", "workingRoot", workingRoot)

	// The health listener restarts on a settings reload when its bind or port
	// changed. Its errors never stop the daemon.
	healthServer := health.NewServer(settings.Health.Bind, settings.Health.Port)
	applyHealthListener(healthServer, settings.Health)

	liveSettings, err := config.NewLiveSettings(settingsPath, settings, func(reloaded *config.Settings) {
		applyHealthListener(healthServer, reloaded.Health)
	})
	if err != nil {
		slog.Error("Failed to watch settings.", "error", err.Error())
		return 1
	}
	liveSettings.Start()
	defer liveSettings.Close()

	slog.Info("Configuration loaded.",
		"repositories", len(settings.Repositories), "watcher", liveSettings.SettingsPath())

	providerFactory, err := forge.NewDefaultFactory()
	if err != nil {
		slog.Error("Failed to initialize providers.", "error", err.Error())
		return 1
	}

	storageFactory := func(settings *config.Settings) (backup.ObjectStorage, error) {
		return store.NewObjectStorage(settings.Storage)
	}
	mirrorStore := backup.NewMirrorStore(workingRoot)
	metadataSync := backup.NewMetadataSyncService(providerFactory)
	gitRepositoryService := git.NewRepositoryService()
	repositorySyncService := backup.NewRepositorySyncService(providerFactory, gitRepositoryService,
		storageFactory, mirrorStore, metadataSync)
	retentionService := backup.NewRetentionService(storageFactory)
	schedulerService := scheduler.NewScheduler(func() *config.Settings {
		return liveSettings.Current()
	}, repositorySyncService, retentionService, healthServer)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		received := <-signalCh
		if received == os.Interrupt {
			slog.Warn("Shutdown requested by Ctrl+C.")
		} else {
			slog.Warn("Shutdown requested by SIGTERM.")
		}
		cancel()
	}()

	slog.Info("Scheduler is running. Press Ctrl+C to stop.")
	runErr := schedulerService.RunForever(ctx)

	if runErr != nil && ctx.Err() == nil {
		slog.Error("Scheduler stopped unexpectedly.", "error", runErr.Error())
		return 1
	}

	slog.Info("Scheduler stopped.")
	healthServer.Stop()
	return 0
}

// applyHealthListener starts, restarts, or stops the status listener to match
// the (possibly reloaded) health settings. A port of zero disables it; a bind
// failure is logged and the daemon runs on without the endpoint.
func applyHealthListener(server *health.Server, health config.Health) {
	server.Restart(health.Bind, health.Port)
}

// resolveSettingsPath returns the settings path: the first CLI argument when
// given, otherwise the default candidate for the environment. Running in a
// container (Docker creates /.dockerenv) defaults to the mounted config
// directory; a native run looks in the working directory.
func resolveSettingsPath(args []string) string {
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		return args[0]
	}

	candidates := defaultSettingsPathCandidates()
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return candidates[0]
}

func defaultSettingsPathCandidates() []string {
	if isRunningInContainer() {
		return []string{filepath.Join(containerConfigPath, "settings.yaml")}
	}
	return []string{"settings.yaml"}
}

// applyRuntimeIdentity takes ownership of the working root and permanently
// drops root privileges to the PUID/PGID identity. When neither variable is
// set the process keeps its current identity, so native runs and operator-
// forced users keep working; a configured but unreachable identity fails
// startup instead of running with the wrong ownership.
func applyRuntimeIdentity(workingRoot string) error {
	identity, configured, err := privilege.FromEnvironment()
	if err != nil {
		return err
	}
	if !configured {
		slog.Debug("PUID and PGID are not set; keeping the current identity.")
		return nil
	}
	if identity.Root() {
		slog.Warn("PUID and PGID are both 0; running as root.")
		return nil
	}
	if err := privilege.Apply(identity, workingRoot); err != nil {
		return err
	}
	slog.Info("Privileges dropped.", "uid", identity.UID, "gid", identity.GID)
	return nil
}

// resolveWorkingRoot picks the root for the mirror cache: the explicit
// override, the persisted data directory in a container, or a temp directory
// outside one.
func resolveWorkingRoot() (string, error) {
	workingRoot := strings.TrimSpace(os.Getenv("GITBACKUP_WORKING_ROOT"))
	if workingRoot == "" {
		// In a container, keep the git mirrors under the persisted data
		// directory so the incremental fetch cache survives restarts and image
		// updates instead of being fully re-cloned every run. Outside a
		// container, fall back to a temp directory.
		if isRunningInContainer() {
			workingRoot = containerDataPath
		} else {
			workingRoot = filepath.Join(os.TempDir(), ".git-backup")
		}
	}
	if err := os.MkdirAll(workingRoot, 0o755); err != nil {
		return "", fmt.Errorf("create working root: %w", err)
	}
	return workingRoot, nil
}

// isRunningInContainer reports whether the process runs inside a container.
// Docker creates /.dockerenv in every container it starts.
func isRunningInContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return false
	}
	return true
}
