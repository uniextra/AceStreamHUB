package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Source represents an external M3U, JSON, scrape, or XMLTV EPG feed.
type Source struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	URL             string  `json:"url"`
	Type            string  `json:"type"`
	IntervalMinutes int     `json:"interval_minutes"`
	LastRun         *string `json:"last_run"`
	Status          string  `json:"status"`
}

// ManualSource represents a manually added channel AceStream ID.
type ManualSource struct {
	Title  string  `json:"title"`
	AceID  string  `json:"ace_id"`
	Extinf *string `json:"extinf"`
}

// Config mirrors the legacy Python config.json schema for drop-in compatibility.
type Config struct {
	Sources           []Source          `json:"sources"`
	ManualSources     []ManualSource    `json:"manual_sources,omitempty"`
	EPGURL            string            `json:"epg_url"`
	Mappings          map[string]string `json:"mappings"`
	Settings          map[string]any    `json:"settings"`
	PreferredIDs      map[string]string `json:"preferred_ids"`
	LastScans         map[string]string `json:"last_scans"`
	BlacklistIDs      []string          `json:"blacklist_ids,omitempty"`
	CustomOrder       []string          `json:"custom_order,omitempty"`
	SecretKey         string            `json:"secret_key,omitempty"`
	AdminPasswordHash string            `json:"admin_password_hash,omitempty"`
	// Legacy migration fields
	LegacySourceURL  string `json:"source_url,omitempty"`
	LegacySourceType string `json:"source_type,omitempty"`
}

// Manager provides thread-safe persistence for config.json and engine_args.txt.
type Manager struct {
	mu         sync.RWMutex
	dataDir    string
	configFile string
}

// NewManager creates a config Manager rooted at dataDir.
func NewManager(dataDir string) *Manager {
	if dataDir == "" {
		dataDir = "."
	}
	return &Manager{
		dataDir:    dataDir,
		configFile: filepath.Join(dataDir, "config.json"),
	}
}

// DataDir returns the configured data directory path.
func (m *Manager) DataDir() string {
	return m.dataDir
}

func defaultConfig() Config {
	return Config{
		Sources:      []Source{},
		Mappings:     map[string]string{},
		Settings:     map[string]any{},
		PreferredIDs: map[string]string{},
		LastScans:    map[string]string{},
	}
}

func normalizeConfig(c *Config) {
	if c.Sources == nil {
		c.Sources = []Source{}
	}
	if c.Mappings == nil {
		c.Mappings = map[string]string{}
	}
	if c.Settings == nil {
		c.Settings = map[string]any{}
	}
	if c.PreferredIDs == nil {
		c.PreferredIDs = map[string]string{}
	}
	if c.LastScans == nil {
		c.LastScans = map[string]string{}
	}
}

// Load reads config.json from disk in a thread-safe manner.
func (m *Manager) Load() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.loadUnlocked()
}

func (m *Manager) loadUnlocked() Config {
	cfg := defaultConfig()
	raw, err := os.ReadFile(m.configFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Error loading config: %v", err)
		}
		return cfg
	}

	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Printf("Error parsing config.json: %v", err)
		return defaultConfig()
	}

	// Migration from very old single-source config format.
	if len(cfg.Sources) == 0 && cfg.LegacySourceURL != "" {
		srcType := cfg.LegacySourceType
		if srcType == "" {
			srcType = "m3u"
		}
		idBytes := make([]byte, 4)
		_, _ = rand.Read(idBytes)
		cfg.Sources = []Source{{
			ID:              hex.EncodeToString(idBytes),
			Name:            "Default Source",
			URL:             cfg.LegacySourceURL,
			Type:            srcType,
			IntervalMinutes: 60,
			LastRun:         nil,
			Status:          "Pending",
		}}
	}

	normalizeConfig(&cfg)
	return cfg
}

// Save writes cfg to config.json atomically.
func (m *Manager) Save(cfg Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveUnlocked(cfg)
}

func (m *Manager) saveUnlocked(cfg Config) error {
	normalizeConfig(&cfg)
	if err := os.MkdirAll(m.dataDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return err
	}
	tmpFile := m.configFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpFile, m.configFile)
}

// Update atomically loads, mutates via fn, and saves config.json.
func (m *Manager) Update(fn func(*Config)) (Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg := m.loadUnlocked()
	fn(&cfg)
	err := m.saveUnlocked(cfg)
	return cfg, err
}

// SetupSecurity ensures a session signing secret exists and honours RESET_PASSWORD.
func (m *Manager) SetupSecurity() Config {
	cfg, err := m.Update(func(c *Config) {
		if c.SecretKey == "" {
			b := make([]byte, 24)
			if _, err := rand.Read(b); err == nil {
				c.SecretKey = hex.EncodeToString(b)
			}
		}
		resetEnv := strings.ToLower(strings.TrimSpace(os.Getenv("RESET_PASSWORD")))
		if (resetEnv == "true" || resetEnv == "1" || resetEnv == "yes") && c.AdminPasswordHash != "" {
			c.AdminPasswordHash = ""
		}
	})
	if err != nil {
		log.Printf("SetupSecurity failed to save config: %v", err)
	}
	return cfg
}

// WriteEngineArgs writes DATA_DIR/engine_args.txt for the acestream-engine container.
func (m *Manager) WriteEngineArgs(cfg Config) error {
	buffer := cfg.EngineLiveBuffer()
	cache := cfg.EngineDiskCache()
	args := fmt.Sprintf("--live-buffer %d", buffer)
	if cache > 0 {
		args += fmt.Sprintf(" --disk-cache-limit %d", int64(cache)*1073741824)
	}
	if err := os.MkdirAll(m.dataDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.dataDir, "engine_args.txt"), []byte(args), 0o644)
}

func (c Config) getBoolSetting(key string, def bool) bool {
	if c.Settings == nil {
		return def
	}
	v, ok := c.Settings[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, err := strconv.ParseBool(t)
		if err == nil {
			return b
		}
	case float64:
		return t != 0
	}
	return def
}

func (c Config) getIntSetting(key string, def int) int {
	if c.Settings == nil {
		return def
	}
	v, ok := c.Settings[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err == nil {
			return n
		}
	}
	return def
}

func (c Config) getStringSetting(key string, def string) string {
	if c.Settings == nil {
		return def
	}
	v, ok := c.Settings[key]
	if !ok || v == nil {
		return def
	}
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

// Language returns the configured UI language ("es" by default).
func (c Config) Language() string { return c.getStringSetting("language", "es") }

// AutoDiscardEnabled reports whether dead streams (>=3 consecutive fails) are skipped.
func (c Config) AutoDiscardEnabled() bool { return c.getBoolSetting("auto_discard_enabled", true) }

// HotFailoverEnabled reports whether mid-stream hot failover is enabled.
func (c Config) HotFailoverEnabled() bool { return c.getBoolSetting("hot_failover_enabled", true) }

// EngineIdleCleanupEnabled reports whether background engine cleanup on idle is enabled.
func (c Config) EngineIdleCleanupEnabled() bool {
	return c.getBoolSetting("engine_idle_cleanup_enabled", true)
}

// EngineIdleTimeout returns idle seconds before engine swarm cleanup (default 60).
func (c Config) EngineIdleTimeout() int { return c.getIntSetting("engine_idle_timeout", 60) }

// StreamTimeout returns upstream connect timeout in seconds (default 30).
func (c Config) StreamTimeout() int { return c.getIntSetting("stream_timeout", 30) }

// HDHREnabled reports whether HDHomeRun emulation endpoints are active.
func (c Config) HDHREnabled() bool { return c.getBoolSetting("hdhr_enabled", false) }

// EngineLiveBuffer returns AceStream live buffer seconds (default 15).
func (c Config) EngineLiveBuffer() int { return c.getIntSetting("engine_live_buffer", 15) }

// EngineDiskCache returns AceStream disk cache limit in GB (default 0).
func (c Config) EngineDiskCache() int { return c.getIntSetting("engine_disk_cache", 0) }

// ScanOnPlayEnabled reports whether tuning triggers background scans.
func (c Config) ScanOnPlayEnabled() bool { return c.getBoolSetting("scan_on_play_enabled", false) }

// ScanOnPlayIntervalMinutes returns minimum minutes between background scans.
func (c Config) ScanOnPlayIntervalMinutes() int {
	return c.getIntSetting("scan_on_play_interval_minutes", 60)
}

// PlaceholderEnabled reports whether placeholder setting is checked in UI.
func (c Config) PlaceholderEnabled() bool { return c.getBoolSetting("placeholder_enabled", false) }
