package checker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProgramLintsJavaScriptWithoutOutputCollision(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.js"), []byte("export const value = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tsconfig.json")
	if err := os.WriteFile(config, []byte(`{"compilerOptions":{"allowJs":true},"files":["main.js"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	program, err := LoadProgram(config)
	if err != nil {
		t.Fatalf("load JavaScript project for linting: %v", err)
	}
	defer program.Close()
	if program.SourceFileByPath(filepath.Join(dir, "main.js")) == nil {
		t.Fatal("JavaScript source was not included in the program")
	}
}
