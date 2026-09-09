package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/neurekadev/git-backup/internal/logging"
)

// pollInterval is how often the settings file is re-hashed for changes. It is
// a variable so tests can shorten it.
//
// Polling, not a filesystem watcher, because the settings file is bind-mounted
// into the container. A single-file bind mount pins the container to the
// original inode, so a host editor that saves by writing a temp file and
// renaming it over the original raises no inotify event and never changes the
// content the container sees. Re-reading the file on an interval detects the
// change wherever the mount does surface it (a directory mount, or an in-place
// edit), on any host and any editor.
var pollInterval = 2 * time.Second

// LiveSettings serves the currently active settings and watches the settings
// file for changes. A failed reload keeps the previous settings running; a
// successful one swaps the current value and re-applies the log level.
type LiveSettings struct {
	settingsPath string
	loader       *Loader
	// onReload, when set, is invoked with every successfully reloaded settings
	// value after the current value has been swapped.
	onReload func(*Settings)

	mu       sync.RWMutex
	current  *Settings
	lastHash []byte

	cancel context.CancelFunc
	done   chan struct{}
}

// NewLiveSettings watches settingsPath, starting from the initial settings the
// caller already loaded. onReload may be nil.
func NewLiveSettings(settingsPath string, initial *Settings, onReload func(*Settings)) (*LiveSettings, error) {
	if initial == nil {
		return nil, errors.New("initial settings are required")
	}

	absolute, err := filepath.Abs(settingsPath)
	if err != nil {
		absolute = settingsPath
	}
	return &LiveSettings{
		settingsPath: absolute,
		loader:       NewLoader(),
		onReload:     onReload,
		current:      initial,
		done:         make(chan struct{}),
	}, nil
}

// SettingsPath is the absolute path of the watched settings file.
func (l *LiveSettings) SettingsPath() string { return l.settingsPath }

// Current returns the active settings value. The value is immutable once
// published, so callers may hold it without copying.
func (l *LiveSettings) Current() *Settings {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.current
}

// Start begins polling for changes. Seeding the baseline from the file the
// initial settings were loaded from means the first poll only reloads when the
// content has actually changed since startup.
func (l *LiveSettings) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	if hash, err := l.computeContentHash(); err == nil {
		l.lastHash = hash
	}

	go l.poll(ctx)
	slog.Info("Watching settings file for changes.", "settingsPath", l.settingsPath)
}

// Close stops polling and waits for the poll goroutine to finish.
func (l *LiveSettings) Close() {
	if l.cancel != nil {
		l.cancel()
	}
	select {
	case <-l.done:
	case <-time.After(5 * time.Second):
	}
}

func (l *LiveSettings) poll(ctx context.Context) {
	defer close(l.done)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.checkForChange()
		}
	}
}

func (l *LiveSettings) checkForChange() {
	currentHash, err := l.computeContentHash()

	// A transient read miss (the file briefly gone during a save) leaves the
	// baseline untouched so the next tick can retry; the existing settings keep
	// running in the meantime.
	if err != nil {
		slog.Debug("Settings file was not readable this poll; keeping current settings.", "settingsPath", l.settingsPath)
		return
	}

	l.mu.Lock()
	unchanged := l.lastHash != nil && bytes.Equal(currentHash, l.lastHash)
	if !unchanged {
		l.lastHash = currentHash
	}
	l.mu.Unlock()
	if unchanged {
		return
	}

	slog.Info("Settings file changed on disk. Reloading.", "settingsPath", l.settingsPath)
	l.reload()
}

// computeContentHash hashes the file's bytes. Read errors are the caller's
// concern; they keep the previous baseline alive.
func (l *LiveSettings) computeContentHash() ([]byte, error) {
	file, err := os.Open(l.settingsPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return nil, err
	}
	return hash.Sum(nil), nil
}

func (l *LiveSettings) reload() {
	result, loadErrors := l.loader.Load(l.settingsPath)
	if len(loadErrors) > 0 {
		slog.Error("Settings reload failed. Existing settings will be kept.", "settingsPath", l.settingsPath)
		for _, loadError := range loadErrors {
			slog.Error("Settings reload validation error.", "error", loadError)
		}
		return
	}

	if level, ok := logging.ParseLevel(result.Logging.LogLevel); ok {
		logging.SetLevel(level)
	}

	l.mu.Lock()
	l.current = result
	l.mu.Unlock()

	slog.Info("Settings reloaded successfully.", "settingsPath", l.settingsPath)
	slog.Debug("Current log level from settings.", "logLevel", result.Logging.LogLevel)

	if l.onReload != nil {
		l.onReload(result)
	}
}
