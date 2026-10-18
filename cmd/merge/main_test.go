package main

import (
	"sort"
	"strings"
	"testing"
)

func TestExtractDropsBareTLD(t *testing.T) {
	cases := []struct {
		name   string
		format string
		input  string
		want   []string
	}{
		{
			name:   "dnsmasq bare tld",
			format: "dnsmasq",
			input: "server=/top/114.114.114.114\n" +
				"server=/wang/114.114.114.114\n" +
				"server=/a.top/114.114.114.114\n",
			want: []string{"a.top"},
		},
		{
			name:   "hosts bare tld",
			format: "hosts",
			input: "0.0.0.0 localhost\n" +
				"0.0.0.0 ads.example.com\n",
			want: []string{"ads.example.com"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := extract(c.format, strings.NewReader(c.input))
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("want %v, got %v", c.want, got)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"Example.COM.", "example.com", true},
		{"a.top", "a.top", true},
		{"top", "", false},
		{"localhost", "", false},
		{"127.0.0.1", "", false},
	}

	for _, c := range cases {
		got, ok := normalize(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("normalize(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
