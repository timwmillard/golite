// Command golite creates a new project from one of golite's samples.
//
//	go install github.com/timwmillard/golite/cmd/golite@latest
//	golite new                 # pick a sample from a menu
//	golite new todo            # into ./todo
//	golite new todo myapp      # into ./myapp, module myapp
//	golite new -module github.com/me/myapp todo myapp
//
// The samples are downloaded from GitHub at the version golite was
// installed at (or -ref). Every directory under samples/ is a sample: its
// description is the first sentence of its package doc comment, and its
// command is the package main nearest its root. The chosen sample's module
// path is replaced with the new one, and go mod tidy pins the golite
// version it was copied from.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"go/doc"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
)

const repo = "github.com/timwmillard/golite"

// file is a file under samples/ in the golite repo.
type file struct {
	Path string // slash-separated, relative to samples/
	Mode fs.FileMode
	Data []byte
}

type sample struct {
	Name  string
	Desc  string
	Run   string // package to go run, relative to the project; "" if none
	Files []file // paths relative to the sample
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "golite:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n  golite new [flags] [sample] [dir]\n\nRun golite new without a sample to pick one from a menu.\n")
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
	if flags.NArg() > 2 {
		flags.Usage()
		return errors.New("too many arguments")
	}

	var files []file
	var err error
	if *from != "" {
		files, err = readLocal(*from)
	} else {
		fmt.Printf("Downloading samples from %s@%s\n", repo, *ref)
		files, err = download(*ref)
	}
	if err != nil {
		return err
	}
	samples, err := discover(files)
	if err != nil {
		return err
	}

	in := bufio.NewReader(os.Stdin)
	var s sample
	var dir string
	if flags.NArg() == 0 {
		if s, err = choose(in, samples); err != nil {
			return err
		}
		if dir, err = ask(in, "Project directory", s.Name); err != nil {
			return err
		}
	} else {
		i := slices.IndexFunc(samples, func(s sample) bool { return s.Name == flags.Arg(0) })
		if i < 0 {
			fmt.Fprintln(os.Stderr, "Samples:")
			list(os.Stderr, samples)
			return fmt.Errorf("unknown sample %q", flags.Arg(0))
		}
		s, dir = samples[i], samples[i].Name
		if flags.NArg() == 2 {
			dir = flags.Arg(1)
		}
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

	fmt.Printf("Creating %s from sample %s\n", dir, s.Name)
	for _, f := range s.Files {
		if err := writeFile(dir, f, s.Name, *module); err != nil {
			return err
		}
	}
	if err := setupModule(dir, *module, *ref, *from); err != nil {
		return err
	}

	fmt.Printf("\nCreated %s. Next:\n\n  cd %s\n", *module, dir)
	if s.Run != "" {
		fmt.Printf("  go run %s\n", s.Run)
	}
	fmt.Println()
	return nil
}

// list prints a numbered list of the samples with their descriptions.
func list(w io.Writer, samples []sample) {
	for i, s := range samples {
		fmt.Fprintf(w, "  %d) %s\n", i+1, s.Name)
		for _, line := range wrap(s.Desc, 72) {
			fmt.Fprintf(w, "       %s\n", line)
		}
	}
}

// wrap splits s into lines of at most width characters, breaking at spaces.
func wrap(s string, width int) []string {
	var lines []string
	line := ""
	for word := range strings.FieldsSeq(s) {
		if line != "" && len(line)+1+len(word) > width {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// choose shows a numbered menu of the samples and reads the choice.
func choose(in *bufio.Reader, samples []sample) (sample, error) {
	fmt.Println("Which sample?")
	list(os.Stdout, samples)
	for {
		answer, err := ask(in, "Choose", "")
		if err != nil {
			return sample{}, err
		}
		for _, s := range samples {
			if s.Name == answer {
				return s, nil
			}
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
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
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

// download returns the files under samples/ in the GitHub archive of ref.
func download(ref string) ([]file, error) {
	url := "https://" + repo + "/archive/" + ref + ".tar.gz"
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	var files []file
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		// Entries are named golite-<ref>/samples/...
		_, p, _ := strings.Cut(h.Name, "/")
		rel, ok := strings.CutPrefix(p, "samples/")
		if !ok || h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files = append(files, file{rel, fs.FileMode(h.Mode), data})
	}
}

// readLocal returns the files under samples/ in the golite checkout at
// root that git would commit: tracked, or untracked but not ignored.
func readLocal(root string) ([]file, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "samples").Output()
	if err != nil {
		return nil, fmt.Errorf("listing samples in %s: %w", root, err)
	}
	var files []file
	for p := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		info, err := os.Lstat(filepath.Join(root, p))
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted but not yet committed
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			return nil, err
		}
		files = append(files, file{strings.TrimPrefix(p, "samples/"), info.Mode(), data})
	}
	return files, nil
}

// discover groups files into samples, one per directory under samples/
// that has Go files, sorted by name.
func discover(files []file) ([]sample, error) {
	byName := map[string]*sample{}
	var samples []*sample
	for _, f := range files {
		name, rel, ok := strings.Cut(f.Path, "/")
		if !ok {
			continue // a file directly in samples/
		}
		s := byName[name]
		if s == nil {
			s = &sample{Name: name}
			byName[name] = s
			samples = append(samples, s)
		}
		f.Path = rel
		s.Files = append(s.Files, f)
	}

	var out []sample
	for _, s := range samples {
		describe(s)
		if s.Desc != "" || s.Run != "" {
			out = append(out, *s)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no samples found")
	}
	slices.SortFunc(out, func(a, b sample) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// describe sets the sample's Run to its shallowest package main, and its
// Desc to the synopsis of its root package's doc comment.
func describe(s *sample) {
	var pkg doc.Package
	for _, f := range s.Files {
		if path.Ext(f.Path) != ".go" || strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f.Path, f.Data, parser.PackageClauseOnly|parser.ParseComments)
		if err != nil {
			continue
		}
		dir := path.Dir(f.Path)
		if af.Name.Name == "main" && (s.Run == "" || depth(dir) < depth(s.Run)) {
			s.Run = dir
		}
		if dir == "." && af.Doc != nil && s.Desc == "" {
			s.Desc = pkg.Synopsis(af.Doc.Text())
		}
	}
	if s.Run != "" && s.Run != "." {
		s.Run = "./" + s.Run
	}
}

// depth is the number of directories in the slash-separated path dir.
func depth(dir string) int {
	if dir == "." {
		return 0
	}
	return strings.Count(dir, "/") + 1
}

// writeFile writes f into dir, replacing the sample's module path with
// module and dropping the replace directive that points the sample at the
// golite checkout.
func writeFile(dir string, f file, name, module string) error {
	if !fs.ValidPath(f.Path) {
		return fmt.Errorf("invalid path %q", f.Path)
	}
	data := bytes.ReplaceAll(f.Data, []byte(repo+"/samples/"+name), []byte(module))
	data = bytes.ReplaceAll(data, []byte("./samples/"+name+" "), []byte(". "))
	if path.Base(f.Path) == "go.mod" {
		data = bytes.ReplaceAll(data, []byte("replace "+repo+" => ../..\n\n"), nil)
		data = bytes.ReplaceAll(data, []byte("replace "+repo+" => ../..\n"), nil)
	}

	p := filepath.Join(dir, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, f.Mode.Perm()|0o600)
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
