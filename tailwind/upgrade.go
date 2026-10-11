package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

const releases = "https://github.com/tailwindlabs/tailwindcss/releases"

var (
	versionRe   = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	versionDecl = regexp.MustCompile(`const version = "[^"]*"`)
	sumsDecl    = regexp.MustCompile(`(?s)var checksums = map\[string\]string\{.*?\n\}`)
)

// upgrade rewrites version and checksums in main.go to the given release,
// or the latest release when want is empty.
func upgrade(want string) error {
	if want == "" {
		latest, err := latestVersion()
		if err != nil {
			return err
		}
		want = latest
	}
	want, err := normVersion(want)
	if err != nil {
		return err
	}
	if want == version {
		fmt.Fprintf(os.Stderr, "tailwind: already at %s\n", version)
		return nil
	}

	sums, err := fetchChecksums(want)
	if err != nil {
		return err
	}

	src, err := sourceFile()
	if err != nil {
		return err
	}
	code, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if !versionDecl.Match(code) || !sumsDecl.Match(code) {
		return fmt.Errorf("%s: version or checksums declaration not found", src)
	}

	names := make([]string, 0, len(sums))
	for name := range sums {
		names = append(names, name)
	}
	sort.Strings(names)
	var m strings.Builder
	m.WriteString("var checksums = map[string]string{\n")
	for _, name := range names {
		fmt.Fprintf(&m, "\t%q: %q,\n", name, sums[name])
	}
	m.WriteString("}")

	code = versionDecl.ReplaceAllLiteral(code, fmt.Appendf(nil, "const version = %q", want))
	code = sumsDecl.ReplaceAllLiteral(code, []byte(m.String()))
	if code, err = format.Source(code); err != nil {
		return err
	}
	if err := os.WriteFile(src, code, 0o644); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "tailwind: upgraded %s -> %s in %s\n", version, want, src)
	fmt.Fprintln(os.Stderr, "tailwind: run `go generate ./...` to rebuild styles.css with the new version")
	return nil
}

// latestVersion reads the tag that /releases/latest redirects to, avoiding
// the rate-limited GitHub API.
func latestVersion() (string, error) {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(releases + "/latest")
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("latest release: no redirect (%s)", resp.Status)
	}
	return path.Base(loc), nil
}

// fetchChecksums returns the release's sha256sums.txt as asset name -> hash.
func fetchChecksums(v string) (map[string]string, error) {
	url := releases + "/download/" + v + "/sha256sums.txt"
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	sums := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "./")
		if strings.HasPrefix(name, "tailwindcss-") {
			sums[name] = fields[0]
		}
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("no tailwindcss binaries listed in %s", url)
	}
	return sums, nil
}

// sourceFile locates main.go, which holds the version and checksums. It
// only works in a golite checkout, not the read-only module cache.
func sourceFile() (string, error) {
	if _, file, _, ok := runtime.Caller(0); ok {
		src := filepath.Join(filepath.Dir(file), "main.go")
		if _, err := os.Stat(src); err == nil {
			return src, nil
		}
	}
	src := filepath.Join("tailwind", "main.go")
	if _, err := os.Stat(src); err != nil {
		return "", errors.New("cannot find tailwind/main.go; run from the golite repo root")
	}
	return src, nil
}
