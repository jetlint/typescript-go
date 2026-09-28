package checker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadProgramRejectsRemovedTypeScript7Option(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.ts"), []byte("export const value = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tsconfig.json")
	if err := os.WriteFile(config, []byte(`{"compilerOptions":{"moduleResolution":"node10"},"files":["main.ts"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	program, err := LoadProgram(config)
	if program != nil {
		program.Close()
		t.Fatal("removed module resolution mode was accepted")
	}
	if err == nil || !strings.Contains(err.Error(), "node10") {
		t.Fatalf("expected a TypeScript config error mentioning node10, got %v", err)
	}
}
