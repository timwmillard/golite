package main

import (
	"os"
	"strings"
	"testing"
)

// TestSamples checks every sample has a description and a command, and is
// listed in the README.
func TestSamples(t *testing.T) {
	files, err := readLocal("../..")
	if err != nil {
		t.Fatal(err)
	}
	samples, err := discover(files)
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		if s.Desc == "" {
			t.Errorf("sample %s: no package doc comment in its root directory", s.Name)
		}
		if s.Run == "" {
			t.Errorf("sample %s: no package main", s.Name)
		}
		if !strings.Contains(string(readme), "(samples/"+s.Name+")") {
			t.Errorf("sample %s: not listed in README.md", s.Name)
		}
	}
}
