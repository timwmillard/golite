package server

import (
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestParseArgs_Precedence(t *testing.T) {
	// .env is read from the working directory.
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("LOG=json\nDATA=from-dotenv\nLOG_LEVEL=warn\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// godotenv sets these in the process env; register them with t.Setenv
	// so they're restored after the test, then unset them for it.
	for _, k := range []string{"DATA", "LOG_LEVEL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	t.Setenv("PORT", "9000")
	t.Setenv("LOG", "text") // real env beats .env
	t.Setenv("RIVERUI_USERNAME", "admin")

	fset := flag.NewFlagSet("test", flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	data := fset.String("data", "data", "app data directory")

	cfg := Config{Port: 7880}
	if err := cfg.ParseArgs(fset, []string{"-port", "9100"}); err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}

	if cfg.Port != 9100 {
		t.Errorf("Port = %d, want 9100 (flag beats env)", cfg.Port)
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want text (env beats .env)", cfg.LogFormat)
	}
	if cfg.LogLevel != slog.LevelWarn {
		t.Errorf("LogLevel = %v, want WARN (from .env)", cfg.LogLevel)
	}
	if *data != "from-dotenv" {
		t.Errorf("app flag -data = %q, want from-dotenv", *data)
	}
	if cfg.RiverUIUsername != "admin" {
		t.Errorf("RiverUIUsername = %q, want admin", cfg.RiverUIUsername)
	}
	if cfg.Logger == nil {
		t.Error("Logger not set")
	}
}

func TestParseArgs_Defaults(t *testing.T) {
	t.Chdir(t.TempDir())

	fset := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := Config{Port: 7880}
	if err := cfg.ParseArgs(fset, nil); err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	if cfg.Port != 7880 {
		t.Errorf("Port = %d, want app default 7880", cfg.Port)
	}
	if cfg.LogFormat != "color" {
		t.Errorf("LogFormat = %q, want color", cfg.LogFormat)
	}
}

func TestParseArgs_InvalidEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PORT", "not-a-number")

	fset := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := Config{}
	if err := cfg.ParseArgs(fset, nil); err == nil {
		t.Error("expected error for invalid $PORT")
	}
}
