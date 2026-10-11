// Command tailwind runs golite's pinned Tailwind CSS standalone CLI,
// downloading it into ./bin on first use. Arguments are passed through to
// tailwindcss.
//
//	go get -tool github.com/timwmillard/golite/tailwind
//	go tool tailwind -i app/styles.css -o app/static/styles.css --minify
//
// An app can use another release, without a golite update, by passing
// -version first:
//
//	go tool tailwind -version v4.4.0 -i app/styles.css -o app/static/styles.css
//
// The first download of a release golite doesn't pin records its checksums,
// from the release's sha256sums.txt, in tailwind.sum in the current
// directory. Commit that file: later downloads, on any machine, are checked
// against it instead of trusting the release again.
//
// To bump the pinned version (latest release if no version is given), from
// a golite checkout:
//
//	go run ./tailwind -upgrade [vX.Y.Z]
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const version = "v4.3.3"

// checksums from https://github.com/tailwindlabs/tailwindcss/releases/download/<version>/sha256sums.txt
var checksums = map[string]string{
	"tailwindcss-linux-arm64":      "55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195",
	"tailwindcss-linux-arm64-musl": "71ea4be79c9de9827545682df3e040053fb535d37c71ed2cfdedf9385a0868e0",
	"tailwindcss-linux-x64":        "dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a",
	"tailwindcss-linux-x64-musl":   "a04d34ceacc8f52cbe8920ad846cdeb61d3d0021dba32db0d1f77c9d9fad7a6c",
	"tailwindcss-macos-arm64":      "cdf646702987a743464dff4d9c60fd4480d1c1e73dd819a9a67f1078815dce9d",
	"tailwindcss-macos-x64":        "7922e0953f2110c05976e3bf58f14e643d90427575e766b7d433f5f80cbee7e1",
	"tailwindcss-windows-x64.exe":  "e0e260ce048014e9268f6237ff18f8ccf02cef521cbd0ae04e82c2cdf7aa3955",
}

// errUnsupported means tailwind publishes no standalone binary for this platform.
var errUnsupported = errors.New("no tailwindcss release")

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-upgrade" || os.Args[1] == "--upgrade") {
		want := ""
		if len(os.Args) > 2 {
			want = os.Args[2]
		}
		if err := upgrade(want); err != nil {
			fmt.Fprintln(os.Stderr, "tailwind:", err)
			os.Exit(1)
		}
		return
	}

	v, args, err := versionFlag(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "tailwind:", err)
		os.Exit(2)
	}

	bin, err := ensure(v)
	if errors.Is(err, errUnsupported) {
		// Fall back to a tailwindcss on PATH (npm, ports, etc.), or skip so the
		// rest of go generate still runs against the checked-in styles.css.
		bin, err = exec.LookPath("tailwindcss")
		if err != nil {
			fmt.Fprintf(os.Stderr, "tailwind: no binary for %s/%s and no tailwindcss on PATH, skipping (using committed styles.css)\n", runtime.GOOS, runtime.GOARCH)
			return
		}
		warnVersion(bin, v)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tailwind:", err)
		os.Exit(1)
	}

	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "tailwind:", err)
		os.Exit(1)
	}
}

// ensure returns the path to release v's binary, downloading it if missing.
func ensure(v string) (string, error) {
	asset, err := assetName()
	if err != nil {
		return "", err
	}

	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	bin := filepath.Join("bin", "tailwindcss-"+v+ext)
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}

	sum, err := checksum(v, asset)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/download/%s/%s", releases, v, asset)
	fmt.Fprintf(os.Stderr, "tailwind: downloading %s\n", url)
	if err := download(url, bin, sum); err != nil {
		return "", err
	}
	return bin, nil
}

func assetName() (string, error) {
	var osName, arch string
	switch runtime.GOOS {
	case "darwin":
		osName = "macos"
	case "linux", "windows":
		osName = runtime.GOOS
	default:
		return "", errUnsupported
	}
	switch runtime.GOARCH {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "arm64"
	default:
		return "", errUnsupported
	}

	name := "tailwindcss-" + osName + "-" + arch
	if runtime.GOOS == "linux" {
		if musl, _ := filepath.Glob("/lib/ld-musl-*"); len(musl) > 0 {
			name += "-musl"
		}
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if _, ok := checksums[name]; !ok {
		return "", errUnsupported
	}
	return name, nil
}

// warnVersion reports when a tailwindcss found on PATH isn't version v.
func warnVersion(bin, v string) {
	out, _ := exec.Command(bin, "--help").CombinedOutput()
	if !strings.Contains(string(out), "tailwindcss "+v) {
		fmt.Fprintf(os.Stderr, "tailwind: using %s, which is not the pinned %s; output may differ\n", bin, v)
	}
}

// download fetches url into dst, verifying its sha256 before moving it into place.
func download(url, dst, sum string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tailwindcss-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", url, got, sum)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
