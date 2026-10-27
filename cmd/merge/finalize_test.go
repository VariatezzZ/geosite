package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VariatezzZ/geosite/internal/geosite"
)

func TestWriteSectionPreservesMultipleAttributes(t *testing.T) {
	var buf bytes.Buffer
	sec := geosite.Section{
		Name: "CATEGORY-ADS-ALL",
		Rules: []geosite.Rule{
			{Type: geosite.RuleDomain, Value: "adash-c.m.taobao.com", Attrs: []string{"telemetry", "ads"}},
			{Type: geosite.RuleFull, Value: "exact.example.com"},
			{Type: geosite.RuleRegexp, Value: `^ads\.example\.com$`, Attrs: []string{"!cn"}},
			{Type: geosite.RuleKeyword, Value: "-ads-"},
		},
	}
	if err := writeSection(&buf, sec); err != nil {
		t.Fatalf("writeSection() error: %v", err)
	}

	want := strings.Join([]string{
		"domain:adash-c.m.taobao.com @ads @telemetry",
		"full:exact.example.com",
		"regexp:^ads\\.example\\.com$ @!cn",
		"keyword:-ads-",
	}, "\n") + "\n"
	if buf.String() != want {
		t.Fatalf("writeSection() = %q, want %q", buf.String(), want)
	}
}

func TestWriteSectionRejectsUnsupportedType(t *testing.T) {
	var buf bytes.Buffer
	sec := geosite.Section{
		Name:  "CN",
		Rules: []geosite.Rule{{Type: geosite.RuleType(99), Value: "example.com"}},
	}
	if err := writeSection(&buf, sec); err == nil {
		t.Fatal("writeSection() succeeded, want error for unsupported rule type")
	}
}

func TestWriteSectionRejectsInvalidAttribute(t *testing.T) {
	var buf bytes.Buffer
	sec := geosite.Section{
		Name:  "CN",
		Rules: []geosite.Rule{{Type: geosite.RuleDomain, Value: "example.com", Attrs: []string{"ads,bad"}}},
	}
	if err := writeSection(&buf, sec); err == nil {
		t.Fatal("writeSection() succeeded, want error for invalid attribute")
	}
}

func TestBuildFinalListCombinesBaseAndKept(t *testing.T) {
	baseDat := map[string]geosite.Section{
		"CATEGORY-ADS-ALL": {
			Name: "CATEGORY-ADS-ALL",
			Rules: []geosite.Rule{
				{Type: geosite.RuleDomain, Value: "base.example"},
			},
		},
	}
	cfg := Config{
		FinalLists: []string{"category-ads-all"},
		Targets: map[string]Target{
			"category-ads-all": {BaseList: "category-ads-all"},
		},
	}
	kept := map[string][]string{"category-ads-all": {"new.example"}}
	targetsByBase := map[string][]string{"category-ads-all": {"category-ads-all"}}

	got, err := buildFinalList(baseDat, "category-ads-all", targetsByBase, cfg, kept)
	if err != nil {
		t.Fatalf("buildFinalList() error: %v", err)
	}
	want := strings.Join([]string{
		"domain:base.example",
		"# --- category-ads-all (auto-generated) ---",
		"domain:new.example",
	}, "\n") + "\n"
	if string(got) != want {
		t.Fatalf("buildFinalList() = %q, want %q", string(got), want)
	}
}

func TestFinalizeDataWritesMultipleAttributes(t *testing.T) {
	basePath := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	baseDat := map[string]geosite.Section{
		"CATEGORY-ADS-ALL": {
			Name: "CATEGORY-ADS-ALL",
			Rules: []geosite.Rule{
				{Type: geosite.RuleDomain, Value: "adash-c.m.taobao.com", Attrs: []string{"ads", "telemetry"}},
			},
		},
	}
	cfg := Config{
		FinalLists: []string{"category-ads-all"},
		Targets: map[string]Target{
			"category-ads-all": {BaseList: "category-ads-all"},
		},
	}

	if err := finalizeData(dataDir, basePath, baseDat, cfg, nil); err != nil {
		t.Fatalf("finalizeData() error: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dataDir, "category-ads-all"))
	if err != nil {
		t.Fatal(err)
	}
	want := "domain:adash-c.m.taobao.com @ads @telemetry\n"
	if string(body) != want {
		t.Fatalf("generated data = %q, want %q", string(body), want)
	}
}

func TestRulesFromSectionCoverage(t *testing.T) {
	rs := rulesFromSection(geosite.Section{
		Name: "CN",
		Rules: []geosite.Rule{
			{Type: geosite.RuleDomain, Value: "example.com"},
			{Type: geosite.RuleFull, Value: "exact.example.net"},
			{Type: geosite.RuleKeyword, Value: "-ads-"},
		},
	})

	if !rs.covered("sub.example.com") {
		t.Fatal("domain rule should cover subdomains")
	}
	if !rs.covered("exact.example.net") {
		t.Fatal("full rule should cover the exact value")
	}
	if rs.covered("sub.exact.example.net") {
		t.Fatal("full rule must not cover subdomains")
	}
}
