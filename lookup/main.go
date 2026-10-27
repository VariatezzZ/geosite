// Command lookup reports which geosite.dat categories a domain belongs to.
//
// It applies the same matching semantics as V2Ray/Xray routing, so the result
// answers the real troubleshooting question "why did this domain go direct /
// proxy?":
//
//	keyword (Plain)  substring match
//	regex   (Regex)  regular expression
//	domain  (Domain) the value itself and any subdomain
//	full    (Full)   exact match only
//
// The repository's shared GeoSite decoder is used for parsing. The compiled
// binary is self-contained and only needs a geosite.dat next to it.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/VariatezzZ/geosite/internal/geosite"
)

// ---- geosite model ----

type rule struct {
	geosite.Rule
	re *regexp.Regexp // compiled once, only for geosite.RuleRegexp
}

type section struct {
	code  string
	rules []rule
}

func typeName(t geosite.RuleType) string {
	switch t {
	case geosite.RuleKeyword:
		return "keyword"
	case geosite.RuleRegexp:
		return "regex"
	case geosite.RuleDomain:
		return "domain"
	case geosite.RuleFull:
		return "full"
	default:
		return fmt.Sprintf("type%d", uint64(t))
	}
}

// loadDat reads a GeoSiteList through the shared decoder, compiles regex rules
// once and drops empty values that could otherwise match everything.
func loadDat(path string) ([]section, error) {
	dat, err := geosite.ReadDat(path)
	if err != nil {
		return nil, err
	}
	secs := make([]section, 0, len(dat))
	for code, sec := range dat {
		rules := make([]rule, 0, len(sec.Rules))
		for _, r := range sec.Rules {
			if r.Value == "" {
				continue
			}
			cr := rule{Rule: r}
			if r.Type == geosite.RuleRegexp {
				cr.re, _ = regexp.Compile(r.Value) // nil disables a bad pattern
			}
			rules = append(rules, cr)
		}
		secs = append(secs, section{code: code, rules: rules})
	}
	return secs, nil
}

// match reports whether the domain satisfies the rule, mirroring V2Ray/Xray.
func (r rule) match(domain string) bool {
	switch r.Type {
	case geosite.RuleFull:
		return domain == r.Value
	case geosite.RuleDomain:
		return domain == r.Value || strings.HasSuffix(domain, "."+r.Value)
	case geosite.RuleRegexp:
		return r.re != nil && r.re.MatchString(domain)
	default: // geosite.RuleKeyword
		return strings.Contains(domain, r.Value)
	}
}

// normalizeHost turns a user-supplied host or URL into a bare lowercase host.
func normalizeHost(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s = strings.TrimPrefix(s, "//")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, ".")
	return s
}

// ---- lookup & output ----

type matchDetail struct {
	Category string `json:"category"`
	Type     string `json:"type"`
	Value    string `json:"value"`
}

type result struct {
	Domain     string        `json:"domain"`
	Categories []string      `json:"categories"`
	Matches    []matchDetail `json:"matches"`
}

// lookup returns every category that contains at least one matching rule.
// A single matching rule per category is enough to mark the category as hit.
func lookup(secs []section, domain string) result {
	r := result{Domain: domain, Categories: []string{}, Matches: []matchDetail{}}
	if domain == "" {
		return r
	}
	for _, s := range secs {
		for _, ru := range s.rules {
			if ru.match(domain) {
				r.Matches = append(r.Matches, matchDetail{
					Category: s.code,
					Type:     typeName(ru.Type),
					Value:    ru.Value,
				})
				break
			}
		}
	}
	sort.Slice(r.Matches, func(i, j int) bool { return r.Matches[i].Category < r.Matches[j].Category })
	for _, m := range r.Matches {
		r.Categories = append(r.Categories, m.Category)
	}
	return r
}

// defaultDatPath points at the geosite.dat sitting next to this executable.
func defaultDatPath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "geosite.dat")
	}
	return "geosite.dat"
}

func main() {
	var (
		datPath = flag.String("dat", defaultDatPath(), "path to geosite.dat (default: alongside this executable)")
		asJSON  = flag.Bool("json", false, "emit machine-readable JSON")
		showWhy = flag.Bool("v", false, "also print the rule (type:value) that matched")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] [domain...]\n\n", filepath.Base(os.Args[0]))
		fmt.Fprintln(os.Stderr, "Report which geosite categories a domain belongs to.")
		fmt.Fprintln(os.Stderr, "With no domain arguments, domains are read from stdin (one per line).")
		fmt.Fprintln(os.Stderr)
		flag.PrintDefaults()
	}
	flag.Parse()

	secs, err := loadDat(*datPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Fatal] %v\n", err)
		os.Exit(1)
	}

	domains := flag.Args()
	if len(domains) == 0 {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				domains = append(domains, line)
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "[Fatal] read stdin: %v\n", err)
			os.Exit(1)
		}
	}
	if len(domains) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	results := make([]result, 0, len(domains))
	for _, raw := range domains {
		results = append(results, lookup(secs, normalizeHost(raw)))
	}

	if *asJSON {
		out, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "[Fatal] encode json: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(out))
		return
	}

	for i, r := range results {
		if i > 0 {
			fmt.Println()
		}
		if len(r.Matches) == 0 {
			fmt.Printf("%s\n  (no match)\n", r.Domain)
			continue
		}
		fmt.Printf("%s\n", r.Domain)
		for _, m := range r.Matches {
			if *showWhy {
				fmt.Printf("  - %-24s %s:%s\n", m.Category, m.Type, m.Value)
			} else {
				fmt.Printf("  - %s\n", m.Category)
			}
		}
	}
}
