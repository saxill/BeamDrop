package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	d := Defaults()
	if d.Port != 4747 {
		t.Errorf("port: %d", d.Port)
	}
	if filepath.Base(d.InboxDir) != "inbox" {
		t.Errorf("inbox: %s", d.InboxDir)
	}
	if filepath.Base(d.KnownPeersDir) != "known_peers" {
		t.Errorf("known: %s", d.KnownPeersDir)
	}
}

func TestEnsureInboxCreates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	d := Defaults()
	inbox, err := EnsureInbox(d)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(inbox)
	if err != nil || !info.IsDir() {
		t.Errorf("inbox not created: %v", err)
	}
}
