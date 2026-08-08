package config

import (
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	ProfileDir            string
	CDPURL                string
	Headless              bool
	ChromeBin             string
	DelayMs               int
	DefaultTimeoutMinutes int
	MaxTimeoutMinutes     int
	DebugDir              string
	Screenshots           bool
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envBool(name string, def bool) bool {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func Load() *Config {
	home, _ := os.UserHomeDir()
	profile := env("CHATGPT_MCP_DIR", filepath.Join(home, ".chatgpt-mcp", "Profile"))
	debug := env("CHATGPT_DEBUG_DIR", filepath.Join(home, ".chatgpt-mcp", "debug"))
	os.MkdirAll(debug, 0o755)

	return &Config{
		ProfileDir:            profile,
		CDPURL:                env("CHATGPT_CDP_URL", ""),
		Headless:              envBool("CHATGPT_HEADLESS", false),
		ChromeBin:             env("CHATGPT_CHROME_BIN", ""),
		DelayMs:               envInt("CHATGPT_DELAY_MS", 1000),
		DefaultTimeoutMinutes: envInt("CHATGPT_TIMEOUT_MINUTES", 30),
		MaxTimeoutMinutes:     envInt("CHATGPT_MAX_TIMEOUT_MINUTES", 120),
		DebugDir:              debug,
		Screenshots:           envBool("CHATGPT_SCREENSHOTS", true),
	}
}
