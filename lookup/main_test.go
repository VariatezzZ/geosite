package main

import (
	"regexp"
	"testing"

	"github.com/VariatezzZ/geosite/internal/geosite"
)

func TestLookupMatchesAllRuleTypes(t *testing.T) {
	secs := []section{
		{code: "TEST", rules: []rule{
			{Rule: geosite.Rule{Type: geosite.RuleKeyword, Value: "-ads-"}},
			{Rule: geosite.Rule{Type: geosite.RuleDomain, Value: "example.com"}},
			{Rule: geosite.Rule{Type: geosite.RuleFull, Value: "exact.example.net"}},
			{
				Rule: geosite.Rule{Type: geosite.RuleRegexp, Value: `^ad[0-9]+\.example$`},
				re:   regexp.MustCompile(`^ad[0-9]+\.example$`),
			},
		}},
	}

	tests := []struct {
		domain string
		want   string
		value  string
	}{
		{"sub.example.com", "domain", "example.com"},
		{"exact.example.net", "full", "exact.example.net"},
		{"ad123.example", "regex", `^ad[0-9]+\.example$`},
		{"my-ads-tracker.test", "keyword", "-ads-"},
	}
	for _, tt := range tests {
		got := lookup(secs, tt.domain)
		if len(got.Matches) != 1 {
			t.Fatalf("lookup(%q) returned %d matches, want 1: %+v", tt.domain, len(got.Matches), got.Matches)
		}
		m := got.Matches[0]
		if m.Category != "TEST" || m.Type != tt.want || m.Value != tt.value {
			t.Fatalf("lookup(%q) = %+v, want type %q value %q", tt.domain, m, tt.want, tt.value)
		}
	}

	if got := lookup(secs, "no-match.test"); len(got.Matches) != 0 {
		t.Fatalf("lookup(no-match.test) = %+v, want no matches", got.Matches)
	}
}
