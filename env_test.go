package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("# comment\nAUTH_KEY=tskey-test\nFOO=bar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vals, err := loadDotEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if vals["AUTH_KEY"] != "tskey-test" {
		t.Fatalf("AUTH_KEY = %q", vals["AUTH_KEY"])
	}
}

func TestResolvePreauthKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("AUTH_KEY=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := resolvePreauthKey("from-flag", path)
	if err != nil || got != "from-flag" {
		t.Fatalf("flag: got %q, err %v", got, err)
	}

	got, err = resolvePreauthKey("", path)
	if err != nil || got != "from-file" {
		t.Fatalf("file: got %q, err %v", got, err)
	}
}
