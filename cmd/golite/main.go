// Command golite creates a new project from one of golite's samples.
//
//	go install github.com/timwmillard/golite/cmd/golite@latest
//	golite new                 # pick a sample from a menu
//	golite new todo            # into ./todo
//	golite new todo myapp      # into ./myapp, module myapp
//	golite new -module github.com/me/myapp todo myapp
//
// The sample is downloaded from GitHub at the version golite was installed
// at (or -ref), its module path is replaced with the new one, and go mod
// tidy pins the golite version it was copied from.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
)

const repo = "github.com/timwmillard/golite"

type sample struct {
	Name string
	Desc string
	Run  string // package to go run, relative to the project
}

var samples = []sample{
	{"server", "River worker, RiverUI and hand-written JSON handlers", "."},
	{"todo", "OpenAPI spec (oapi-codegen strict server) plus sqlc queries", "./cmd/todoserver"},
	{"multitenant", "The todo API with a database per company, using package tenant", "./cmd/multitenantserver"},
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "golite:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n  golite new [flags] [sample] [dir]\n\nSamples:\n")
	for _, s := range samples {
		fmt.Fprintf(os.Stderr, "  %-12s %s\n", s.Name, s.Desc)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "new" {
		usage()
		if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
			return nil
		}
		return fmt.Errorf("unknown command %q", args[0])
	}

	flags := flag.NewFlagSet("new", flag.ExitOnError)
	module := flags.String("module", "", "module path of the new project (default: the directory name)")
	ref := flags.String("ref", defaultRef(), "golite git ref (tag, branch or commit) to copy the sample from")
	from := flags.String("from", "", "copy from a local golite checkout instead of downloading")
	flags.Usage = func() { usage(); fmt.Fprintln(os.Stderr, "\nFlags:"); flags.PrintDefaults() }
	flags.Parse(args[1:])

	in := bufio.NewReader(os.Stdin)
	var s sample
	var dir string
	switch flags.NArg() {
	case 0:
		var err error
		if s, err = choose(in); err != nil {
			return err
		}
		if dir, err = ask(in, "Project directory", s.Name); err != nil {
			return err
		}
	case 1, 2:
		var ok bool
		if s, ok = find(flags.Arg(0)); !ok {
			usage()
			return fmt.Errorf("unknown sample %q", flags.Arg(0))
		}
		dir = s.Name
		if flags.NArg() == 2 {
			dir = flags.Arg(1)
		}
	default:
		flags.Usage()
		return errors.New("too many arguments")
	}

	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists", dir)
	}
	if *module == "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		*module = filepath.Base(abs)
	}

	if *from != "" {
		fmt.Printf("Copying %s from %s into %s\n", s.Name, *from, dir)
		err := copyLocal(filepath.Join(*from, "samples", s.Name), dir)
		if err != nil {
			return err
		}
	} else {
		fmt.Printf("Downloading %s from %s@%s into %s\n", s.Name, repo, *ref, dir)
		if err := download(*ref, s.Name, dir); err != nil {
			os.RemoveAll(dir)
			return err
		}
	}
	if err := rewrite(dir, s.Name, *module); err != nil {
		return err
	}
	if err := setupModule(dir, *module, *ref, *from); err != nil {
		return err
	}

	fmt.Printf("\nCreated %s. Next:\n\n  cd %s\n  go run %s\n\n", *module, dir, s.Run)
	return nil
}

func find(name string) (sample, bool) {
	for _, s := range samples {
		if s.Name == name {
			return s, true
		}
	}
	return sample{}, false
}

// choose shows a numbered menu of the samples and reads the choice.
func choose(in *bufio.Reader) (sample, error) {
	fmt.Println("Which sample?")
	for i, s := range samples {
		fmt.Printf("  %d) %-12s %s\n", i+1, s.Name, s.Desc)
	}
	for {
		answer, err := ask(in, "Choose", "1")
		if err != nil {
			return sample{}, err
		}
		if s, ok := find(answer); ok {
			return s, nil
		}
		var n int
		if _, err := fmt.Sscan(answer, &n); err == nil && n >= 1 && n <= len(samples) {
			return samples[n-1], nil
		}
		fmt.Printf("Enter a number from 1 to %d\n", len(samples))
	}
}

// ask prompts for a line of input, returning def when it's blank.
func ask(in *bufio.Reader, prompt, def string) (string, error) {
	fmt.Printf("%s [%s]: ", prompt, def)
	line, err := in.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", errors.New("no input")
	}
	if line = strings.TrimSpace(line); line == "" {
		return def, nil
	}
	return line, nil
}

// pseudoVersion matches the commit hash at the end of a Go pseudo-version.
var pseudoVersion = regexp.MustCompile(`-([0-9a-f]{12})$`)

// defaultRef is the version golite was installed at, so the sample matches
// the golite version it'll depend on. Local builds use main.
func defaultRef() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "main"
	}
	v, _, _ := strings.Cut(info.Main.Version, "+") // drop +dirty
	if v == "" || v == "(devel)" {
		return "main"
	}
	if m := pseudoVersion.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return v
}

// download extracts samples/<name>/ from the GitHub archive of ref into dir.
func download(ref, name, dir string) error {
	url := "https://" + repo + "/archive/" + ref + ".tar.gz"
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// Entries are named golite-<ref>/samples/<name>/...
		_, p, _ := strings.Cut(h.Name, "/")
		rel, ok := strings.CutPrefix(p, "samples/"+name+"/")
		if !ok || rel == "" || h.Typeflag != tar.TypeReg {
			continue
		}
		found = true
		if err := writeFile(dir, rel, tr, fs.FileMode(h.Mode)); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("sample %s not found in %s@%s", name, repo, ref)
	}
	return nil
}

// copyLocal copies src into dir, skipping data directories and dotfiles
// other than .gitignore.
func copyLocal(src, dir string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		name := d.Name()
		if rel != "." && (name == "data" || name == "tmp" || strings.HasPrefix(name, ".") && name != ".gitignore") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return writeFile(dir, filepath.ToSlash(rel), f, info.Mode())
	})
}

func writeFile(dir, rel string, r io.Reader, mode fs.FileMode) error {
	if !fs.ValidPath(rel) {
		return fmt.Errorf("invalid path %q", rel)
	}
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm()|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// rewrite replaces the sample's module path with module in every file, and
// drops the replace directive that points the sample at the golite checkout.
func rewrite(dir, name, module string) error {
	oldModule := repo + "/samples/" + name
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out := bytes.ReplaceAll(b, []byte(oldModule), []byte(module))
		out = bytes.ReplaceAll(out, []byte("./samples/"+name+" "), []byte(". "))
		if d.Name() == "go.mod" {
			out = bytes.ReplaceAll(out, []byte("replace "+repo+" => ../..\n\n"), nil)
			out = bytes.ReplaceAll(out, []byte("replace "+repo+" => ../..\n"), nil)
		}
		if bytes.Equal(b, out) {
			return nil
		}
		return os.WriteFile(p, out, 0)
	})
}

// setupModule creates go.mod if the sample lives in golite's own module,
// requires golite (the checkout with -from, else ref) and tidies it.
func setupModule(dir, module, ref, from string) error {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); errors.Is(err, fs.ErrNotExist) {
		if err := goCmd(dir, "mod", "init", module); err != nil {
			return err
		}
	}
	if from != "" {
		abs, err := filepath.Abs(from)
		if err != nil {
			return err
		}
		err = goCmd(dir, "mod", "edit", "-replace", repo+"="+abs, "-require", repo+"@v0.0.0-00010101000000-000000000000")
		if err != nil {
			return err
		}
	} else {
		// Drop the sample's placeholder version, which only resolved
		// through the replace directive.
		if err := goCmd(dir, "mod", "edit", "-droprequire", repo); err != nil {
			return err
		}
		if err := goCmd(dir, "get", repo+"@"+ref); err != nil {
			return err
		}
	}
	// Requiring golite first means tidy picks the versions golite's go.mod
	// requires (e.g. River, which must match riversqlite), not the latest.
	return goCmd(dir, "mod", "tidy")
}

func goCmd(dir string, args ...string) error {
	fmt.Println("go", strings.Join(args, " "))
	cmd := exec.Command("go", args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s: %w", args[0], err)
	}
	return nil
}
