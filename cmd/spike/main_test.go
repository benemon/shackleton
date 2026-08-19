package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFilePreservesExistingEnvironment(t *testing.T) {
	t.Setenv("SHACKLETON_EXISTING", "from-environment")
	t.Setenv("SHACKLETON_LOADED", "")
	path := filepath.Join(t.TempDir(), "test.env")
	if err := os.WriteFile(path, []byte("# comment\nSHACKLETON_EXISTING=from-file\nSHACKLETON_LOADED='value with spaces'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("SHACKLETON_EXISTING"); got != "from-environment" {
		t.Fatalf("existing environment was overwritten: %q", got)
	}
	if got := os.Getenv("SHACKLETON_LOADED"); got != "value with spaces" {
		t.Fatalf("dotenv value was not loaded: %q", got)
	}
}
