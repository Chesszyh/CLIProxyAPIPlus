package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	defaultPersistenceDebounce = 2 * time.Second
	persistedSnapshotVersion   = 1
	persistedSnapshotFileName  = "usage-stats.json"
)

type persistedSnapshot struct {
	Version   int                `json:"version"`
	UpdatedAt time.Time          `json:"updated_at,omitempty"`
	Usage     StatisticsSnapshot `json:"usage"`
}

type FilePersistence struct {
	stats *RequestStatistics

	mu       sync.Mutex
	path     string
	debounce time.Duration
	timer    *time.Timer
	seq      uint64
	flushed  uint64
}

func NewFilePersistence(stats *RequestStatistics) *FilePersistence {
	return &FilePersistence{
		stats:    stats,
		debounce: defaultPersistenceDebounce,
	}
}

func (p *FilePersistence) Configure(path string) error {
	cleaned := strings.TrimSpace(path)
	if cleaned == "" {
		p.mu.Lock()
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		p.path = ""
		p.seq = 0
		p.flushed = 0
		p.mu.Unlock()
		return nil
	}
	cleaned = filepath.Clean(cleaned)

	preTotal := int64(0)
	if p != nil && p.stats != nil {
		preTotal = p.stats.Snapshot().TotalRequests
	}

	p.mu.Lock()
	if p.path == cleaned {
		p.mu.Unlock()
		return nil
	}
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.path = cleaned
	p.seq = 0
	p.flushed = 0
	p.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(cleaned), 0o755); err != nil {
		return fmt.Errorf("usage persistence: create directory: %w", err)
	}

	payload, exists, err := readPersistedSnapshot(cleaned)
	if err != nil {
		return err
	}
	if exists && p.stats != nil {
		result := p.stats.MergeSnapshot(payload.Usage)
		log.Infof("usage statistics restored from %s: added=%d skipped=%d", cleaned, result.Added, result.Skipped)
	} else {
		log.Infof("usage statistics persistence enabled: %s", cleaned)
	}

	postTotal := int64(0)
	if p != nil && p.stats != nil {
		postTotal = p.stats.Snapshot().TotalRequests
	}
	if (preTotal > 0 || !exists) && postTotal > 0 {
		p.NotifyChanged()
	}
	return nil
}

func (p *FilePersistence) NotifyChanged() {
	if p == nil {
		return
	}

	p.mu.Lock()
	if p.path == "" {
		p.mu.Unlock()
		return
	}
	p.seq++
	if p.debounce <= 0 {
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		p.mu.Unlock()
		if err := p.Flush(); err != nil {
			log.WithError(err).Warn("usage statistics flush failed")
		}
		return
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(p.debounce, func() {
		if err := p.Flush(); err != nil {
			log.WithError(err).Warn("usage statistics flush failed")
		}
	})
	p.mu.Unlock()
}

func (p *FilePersistence) Flush() error {
	if p == nil {
		return nil
	}

	p.mu.Lock()
	path := p.path
	targetSeq := p.seq
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	if path == "" || p.stats == nil || targetSeq == p.flushed {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	payload := persistedSnapshot{
		Version:   persistedSnapshotVersion,
		UpdatedAt: time.Now().UTC(),
		Usage:     p.stats.Snapshot(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("usage persistence: marshal snapshot: %w", err)
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return err
	}

	p.mu.Lock()
	if p.path == path && p.flushed < targetSeq {
		p.flushed = targetSeq
	}
	p.mu.Unlock()
	return nil
}

func (p *FilePersistence) Close() error {
	if p == nil {
		return nil
	}
	if err := p.Flush(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.path = ""
	p.seq = 0
	p.flushed = 0
	p.mu.Unlock()
	return nil
}

func ResolvePersistencePath(cfg *config.Config, configFilePath string) string {
	if base := util.WritablePath(); strings.TrimSpace(base) != "" {
		return filepath.Join(base, "state", persistedSnapshotFileName)
	}
	if cfg != nil {
		if authDir, err := util.ResolveAuthDir(cfg.AuthDir); err == nil && strings.TrimSpace(authDir) != "" {
			return filepath.Join(authDir, "state", persistedSnapshotFileName)
		}
	}
	if userConfigDir, err := os.UserConfigDir(); err == nil && strings.TrimSpace(userConfigDir) != "" {
		return filepath.Join(userConfigDir, "cliproxyapi", "state", persistedSnapshotFileName)
	}
	if homeDir, err := os.UserHomeDir(); err == nil && strings.TrimSpace(homeDir) != "" {
		return filepath.Join(homeDir, ".cliproxyapi", "state", persistedSnapshotFileName)
	}
	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		return filepath.Join("state", persistedSnapshotFileName)
	}
	base := configFilePath
	if info, err := os.Stat(configFilePath); err == nil && !info.IsDir() {
		base = filepath.Dir(configFilePath)
	}
	return filepath.Join(base, "state", persistedSnapshotFileName)
}

func readPersistedSnapshot(path string) (persistedSnapshot, bool, error) {
	var payload persistedSnapshot

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return payload, false, nil
		}
		return payload, false, fmt.Errorf("usage persistence: read snapshot: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return payload, false, nil
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return payload, true, fmt.Errorf("usage persistence: parse snapshot: %w", err)
	}
	if payload.Version != 0 && payload.Version != persistedSnapshotVersion {
		return payload, true, fmt.Errorf("usage persistence: unsupported snapshot version %d", payload.Version)
	}
	return payload, true, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("usage persistence: create directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".usage-stats-*.tmp")
	if err != nil {
		return fmt.Errorf("usage persistence: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("usage persistence: write temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("usage persistence: chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("usage persistence: close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("usage persistence: replace snapshot: %w", err)
	}
	return nil
}

var defaultFilePersistence = NewFilePersistence(defaultRequestStatistics)

func ConfigurePersistence(cfg *config.Config, configFilePath string) error {
	return defaultFilePersistence.Configure(ResolvePersistencePath(cfg, configFilePath))
}

func FlushPersistence() error {
	return defaultFilePersistence.Flush()
}

func ClosePersistence() error {
	return defaultFilePersistence.Close()
}

func notifyRequestStatisticsChanged(stats *RequestStatistics) {
	if stats == nil || stats != defaultRequestStatistics {
		return
	}
	defaultFilePersistence.NotifyChanged()
}
