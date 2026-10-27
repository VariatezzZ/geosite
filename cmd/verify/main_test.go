package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePlainList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gfwlist.txt")
	body := strings.Join([]string{
		"# comment",
		"domain:plain.example",
		"full:full.example",
		"keyword:-ads-",
		"regexp:^a:b$ @ads",
		"domain:foo.example:@ads",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := parsePlainList(path)
	if err != nil {
		t.Fatalf("parsePlainList() error: %v", err)
	}
	want := []string{"plain.example", "full.example", "-ads-", "^a:b$", "foo.example"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parsePlainList() = %v, want %v", got, want)
	}
}
