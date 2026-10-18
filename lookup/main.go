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
// The program is a standalone module: it embeds a tiny hand-rolled protobuf
// wire reader instead of depending on v2ray-core, so this directory can be
// copied elsewhere and built with nothing but the Go standard library.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ---- minimal protobuf wire reader ----

type wireField struct {
	num  int
	wtyp int
	vi   uint64
	data []byte
}

func readVarint(b []byte, i int) (uint64, int, error) {
	var v uint64
	var shift uint
	for {
		if i >= len(b) {
			return 0, i, errors.New("truncated varint")
		}
		c := b[i]
		i++
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, i, nil
		}
		shift += 7
		if shift > 63 {
			return 0, i, errors.New("varint overflow")
		}
	}
}

func parseFields(b []byte) ([]wireField, error) {
	var out []wireField
	i := 0
	for i < len(b) {
		key, ni, err := readVarint(b, i)
		if err != nil {
			return nil, err
		}
		i = ni
		f := wireField{num: int(key >> 3), wtyp: int(key & 7)}
		switch f.wtyp {
		case 0:
			v, ni, err := readVarint(b, i)
			if err != nil {
				return nil, err
			}
			i = ni
			f.vi = v
		case 2:
			l, ni, err := readVarint(b, i)
			if err != nil {
				return nil, err
			}
			i = ni
			if i+int(l) > len(b) {
				return nil, errors.New("length-delimited field overflow")
			}
			f.data = b[i : i+int(l)]
			i += int(l)
		case 1:
			i += 8
		case 5:
			i += 4
		default:
			return nil, fmt.Errorf("unsupported wire type %d", f.wtyp)
		}
		out = append(out, f)
	}
	return out, nil
}

// ---- geosite model ----

// Domain.Type wire values, matching v2ray-core's GeoSite proto.
const (
	typPlain  = 0 // keyword: substring
	typRegex  = 1 // regular expression
	typDomain = 2 // the value itself and all subdomains
	typFull   = 3 // exact match only
)

type rule struct {
	typ   uint64
	value string
	re    *regexp.Regexp // compiled once, only for typRegex
}

type section struct {
	code  string
	rules []rule
}

func typeName(t uint64) string {
	switch t {
	case typPlain:
		return "keyword"
	case typRegex:
		return "regex"
	case typDomain:
		return "domain"
	case typFull:
		return "full"
	default:
		return fmt.Sprintf("type%d", t)
	}
}

// parseDat reads a GeoSiteList and returns one section per category, keeping
// both the rule kind and its value (unlike cmd/verify, which only needs values).
func parseDat(path string) ([]section, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	top, err := parseFields(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}

	var secs []section
	for _, f := range top {
		if f.num != 1 || f.wtyp != 2 { // GeoSiteList.entry
			continue
		}
		sf, err := parseFields(f.data)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", path, err)
		}
		var code string
		var rules []rule
		for _, g := range sf {
			switch {
			case g.num == 1 && g.wtyp == 2: // GeoSite.country_code
				code = string(g.data)
			case g.num == 2 && g.wtyp == 2: // GeoSite.domain
				df, err := parseFields(g.data)
				if err != nil {
					return nil, fmt.Errorf("parse %q: %w", path, err)
				}
				var r rule
				for _, x := range df {
					if x.num == 1 && x.wtyp == 0 {
						r.typ = x.vi
					}
					if x.num == 2 && x.wtyp == 2 {
						r.value = string(x.data)
					}
				}
				if r.value == "" {
					continue
				}
				if r.typ == typRegex {
					r.re, _ = regexp.Compile(r.value) // nil disables a bad pattern
				}
				rules = append(rules, r)
			}
		}
		if code == "" {
			continue
		}
		secs = append(secs, section{code: code, rules: rules})
	}
	return secs, nil
}

// match reports whether the domain satisfies the rule, mirroring V2Ray/Xray.
func (r rule) match(domain string) bool {
	switch r.typ {
	case typFull:
		return domain == r.value
	case typDomain:
		return domain == r.value || strings.HasSuffix(domain, "."+r.value)
	case typRegex:
		return r.re != nil && r.re.MatchString(domain)
	default: // typPlain
		return strings.Contains(domain, r.value)
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
					Type:     typeName(ru.typ),
					Value:    ru.value,
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

	secs, err := parseDat(*datPath)
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
