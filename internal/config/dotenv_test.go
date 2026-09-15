package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotenv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# comment\nexport GITHUB_TOKEN=\"secret123\"\nPORT=9090\n\nEMPTY=\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PORT", "8080") // pre-set: must NOT be overridden
	os.Unsetenv("GITHUB_TOKEN")

	if err := LoadDotenv(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := os.Getenv("GITHUB_TOKEN"); got != "secret123" {
		t.Errorf("GITHUB_TOKEN = %q, want secret123", got)
	}
	if got := os.Getenv("PORT"); got != "8080" {
		t.Errorf("PORT = %q, want 8080 (real env should win)", got)
	}
}

func TestLoadDotenv_MissingFileOK(t *testing.T) {
	if err := LoadDotenv(filepath.Join(t.TempDir(), "nope.env")); err != nil {
		t.Errorf("missing file should be ok, got %v", err)
	}
}
