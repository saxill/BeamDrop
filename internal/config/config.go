// Package config holds shared defaults and path resolution.
package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	Port int
	// ConfigDir holds everything beamdrop persists that is not a received
	// file: the known-peers store and the web UI's TLS keypair.
	ConfigDir     string
	InboxDir      string
	KnownPeersDir string
}

func Defaults() Config {
	home, _ := os.UserHomeDir()
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" && home != "" {
		xdg = filepath.Join(home, ".config")
	}
	configDir := filepath.Join(xdg, "beamdrop")
	return Config{
		Port:          4747,
		ConfigDir:     configDir,
		InboxDir:      filepath.Join(home, "Portal", "inbox"),
		KnownPeersDir: filepath.Join(configDir, "known_peers"),
	}
}

func EnsureInbox(c Config) (string, error) {
	if err := os.MkdirAll(c.InboxDir, 0o755); err != nil {
		return "", err
	}
	return c.InboxDir, nil
}
