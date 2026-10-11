package main

import (
	"os"
	"slices"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	tests := []struct {
		args     []string
		wantV    string
		wantArgs []string
		wantErr  bool
	}{
		{nil, version, nil, false},
		{[]string{"-i", "in.css"}, version, []string{"-i", "in.css"}, false},
		{[]string{"-version", "v4.1.0", "-i", "in.css"}, "v4.1.0", []string{"-i", "in.css"}, false},
		{[]string{"--version=4.1.0", "-i", "in.css"}, "v4.1.0", []string{"-i", "in.css"}, false},
		{[]string{"-version"}, "", nil, true},
		{[]string{"-version", "latest"}, "", nil, true},
	}
	for _, tt := range tests {
		v, args, err := versionFlag(tt.args)
		if (err != nil) != tt.wantErr {
			t.Errorf("versionFlag(%q) err = %v, want error %v", tt.args, err, tt.wantErr)
			continue
		}
		if v != tt.wantV || !slices.Equal(args, tt.wantArgs) {
			t.Errorf("versionFlag(%q) = %q, %q, want %q, %q", tt.args, v, args, tt.wantV, tt.wantArgs)
		}
	}
}

func TestSums(t *testing.T) {
	t.Chdir(t.TempDir())

	if sums, err := readSums("v4.1.0"); err != nil || sums != nil {
		t.Fatalf("readSums with no file = %v, %v, want nil, nil", sums, err)
	}

	a := map[string]string{"tailwindcss-macos-arm64": "aaa", "tailwindcss-linux-x64": "bbb"}
	b := map[string]string{"tailwindcss-macos-arm64": "ccc"}
	if err := appendSums("v4.1.0", a); err != nil {
		t.Fatal(err)
	}
	if err := appendSums("v4.2.0", b); err != nil {
		t.Fatal(err)
	}

	for v, want := range map[string]map[string]string{"v4.1.0": a, "v4.2.0": b} {
		got, err := readSums(v)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Errorf("readSums(%s) = %v, want %v", v, got, want)
		}
		for name, sum := range want {
			if got[name] != sum {
				t.Errorf("readSums(%s)[%s] = %q, want %q", v, name, got[name], sum)
			}
		}
	}

	// The pinned release never reads the file.
	if err := os.WriteFile(sumFile, []byte(version+" tailwindcss-macos-arm64 bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sum, _ := checksum(version, "tailwindcss-macos-arm64"); sum != checksums["tailwindcss-macos-arm64"] {
		t.Errorf("checksum(pinned) = %q, want the pinned checksum", sum)
	}
}
