package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// sumFile records the checksums of releases golite doesn't pin, one
// "<version> <asset> <sha256>" per line, so they're only trusted once.
const sumFile = "tailwind.sum"

// versionFlag takes a leading -version vX.Y.Z (or -version=vX.Y.Z) off
// args, returning the release to run (the pinned one if absent) and the
// arguments for tailwindcss.
func versionFlag(args []string) (string, []string, error) {
	if len(args) == 0 {
		return version, args, nil
	}
	name, val, hasVal := strings.Cut(args[0], "=")
	if name != "-version" && name != "--version" {
		return version, args, nil
	}
	rest := args[1:]
	if !hasVal {
		if len(rest) == 0 {
			return "", nil, errors.New("-version needs a value, e.g. -version v4.1.0")
		}
		val, rest = rest[0], rest[1:]
	}
	v, err := normVersion(val)
	if err != nil {
		return "", nil, err
	}
	return v, rest, nil
}

// normVersion adds a missing "v" and checks v looks like vX.Y.Z.
func normVersion(v string) (string, error) {
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !versionRe.MatchString(v) {
		return "", fmt.Errorf("invalid version %q, want vX.Y.Z", v)
	}
	return v, nil
}

// checksum returns asset's sha256 for release v: the pinned one, else the
// one in tailwind.sum, else the release's own, which is then recorded in
// tailwind.sum.
func checksum(v, asset string) (string, error) {
	if v == version {
		return checksums[asset], nil
	}

	sums, err := readSums(v)
	if err != nil {
		return "", err
	}
	if sums == nil {
		if sums, err = fetchChecksums(v); err != nil {
			return "", err
		}
		if err := appendSums(v, sums); err != nil {
			return "", err
		}
		fmt.Fprintf(os.Stderr, "tailwind: recorded checksums for %s in %s; commit it\n", v, sumFile)
	}

	sum, ok := sums[asset]
	if !ok {
		return "", fmt.Errorf("%w %s for %s", errUnsupported, v, asset)
	}
	return sum, nil
}

// readSums returns release v's checksums from tailwind.sum, or nil if it has
// none.
func readSums(v string) (map[string]string, error) {
	f, err := os.Open(sumFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var sums map[string]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 3 || fields[0] != v {
			continue
		}
		if sums == nil {
			sums = map[string]string{}
		}
		sums[fields[1]] = fields[2]
	}
	return sums, sc.Err()
}

// appendSums adds release v's checksums to tailwind.sum.
func appendSums(v string, sums map[string]string) error {
	names := make([]string, 0, len(sums))
	for name := range sums {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s %s %s\n", v, name, sums[name])
	}

	f, err := os.OpenFile(sumFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
