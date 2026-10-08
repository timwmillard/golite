package server

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"github.com/joho/godotenv"

	"github.com/timwmillard/golite/colorlog"
)

// Parse fills c from, in increasing precedence: the values already in c
// (the app's defaults), a .env file, environment variables, and finally
// command-line flags. It then builds c.Logger and makes it the slog default.
//
// It registers -port, -log and -log-level on flag.CommandLine. Every flag
// on flag.CommandLine, including ones the app registers before calling
// Parse, can also be set by an env var named after it: -port is PORT,
// -log-level is LOG_LEVEL. A flag given on the command line always wins.
//
// The .env file is loaded into the process environment, so the app's own
// os.Getenv calls after Parse see it too. It never overrides a variable
// that's already set.
func (c *Config) Parse() error {
	return c.ParseArgs(flag.CommandLine, os.Args[1:])
}

// ParseArgs is Parse with an explicit flag set and arguments.
func (c *Config) ParseArgs(fset *flag.FlagSet, args []string) error {
	fset.IntVar(&c.Port, "port", cmp.Or(c.Port, 8080), "HTTP listen port")
	fset.StringVar(&c.LogFormat, "log", cmp.Or(c.LogFormat, "color"), "log format: color, text or json")
	fset.TextVar(&c.LogLevel, "log-level", c.LogLevel, "minimum log level: debug, info, warn or error")

	fset.VisitAll(func(f *flag.Flag) {
		f.Usage += fmt.Sprintf(" [$%s]", envName(f.Name))
	})

	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("load .env: %w", err)
	}

	if err := fset.Parse(args); err != nil {
		return err
	}

	if err := setFlagsFromEnv(fset); err != nil {
		return err
	}

	c.RiverUIUsername = cmp.Or(c.RiverUIUsername, os.Getenv("RIVERUI_USERNAME"))
	c.RiverUIPassword = cmp.Or(c.RiverUIPassword, os.Getenv("RIVERUI_PASSWORD"))

	logger, err := NewLogger(os.Stderr, c.LogFormat, c.LogLevel)
	if err != nil {
		return err
	}
	c.Logger = logger
	slog.SetDefault(logger)

	return nil
}

// setFlagsFromEnv sets every flag not given on the command line from its
// env var, if that's set.
func setFlagsFromEnv(fset *flag.FlagSet) error {
	given := map[string]bool{}
	fset.Visit(func(f *flag.Flag) { given[f.Name] = true })

	var errs []error
	fset.VisitAll(func(f *flag.Flag) {
		if given[f.Name] {
			return
		}
		name := envName(f.Name)
		if v, ok := os.LookupEnv(name); ok {
			if err := f.Value.Set(v); err != nil {
				errs = append(errs, fmt.Errorf("invalid value %q for $%s: %w", v, name, err))
			}
		}
	})
	return errors.Join(errs...)
}

// envName maps a flag name to its env var: "log-level" is "LOG_LEVEL".
func envName(flagName string) string {
	return strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// NewLogger returns a logger writing to w in the given format: "color"
// (also "human"), "text" or "json".
func NewLogger(w io.Writer, format string, level slog.Level) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: level}
	switch format {
	case "color", "human", "":
		return slog.New(colorlog.NewHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q (want color, text or json)", format)
	}
}
