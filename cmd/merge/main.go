// Command merge merges external domain lists (China domains, ad domains) into a
// freshly checked out v2fly/domain-list-community data directory.
//
// Design notes:
//   - The base data is NOT re-parsed here. Instead we consume the flattened,
//     already-deduplicated lists exported by the upstream builder
//     (`go run ./ --exportlists=...`). This reuses upstream's include resolution,
//     attribute filtering, full/domain precedence and parent-domain de-duplication.
//   - Every candidate domain is skipped when it is already covered by the base
//     list, and for the "cn" target also when it is covered by geolocation-!cn
//     (conflict policy "base wins": such domains stay in the proxy list and are
//     never added to cn).
//   - Only plain `domain:` rules are emitted, so anything we add stays compatible
//     with upstream's syntax validation.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxDomainLen = 253
	maxLabelLen  = 63
	userAgent    = "geosite-builder/1.0"
)

type Target struct {
	BaseList     string   `json:"base_list"`
	ExcludeLists []string `json:"exclude_lists"`
}

type Source struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Format string `json:"format"`
	Target string `json:"target"`
}

type Config struct {
	Targets map[string]Target `json:"targets"`
	Sources []Source          `json:"sources"`
}

// ruleSet is the flattened view of one base list.
type ruleSet struct {
	roots map[string]struct{} // `domain:` values (match the value itself and all subdomains)
	fulls map[string]struct{} // `full:` values (exact match only)
}

func newRuleSet() *ruleSet {
	return &ruleSet{roots: map[string]struct{}{}, fulls: map[string]struct{}{}}
}

// parseExported reads a plaintext list exported by the upstream builder.
// Line format is `type:value` optionally followed by `:@attr1,@attr2`.
func parseExported(path string) (*ruleSet, error) {
	rs := newRuleSet()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		typ, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value := rest
		if idx := strings.Index(rest, ":"); idx >= 0 { // strip trailing attributes
			value = rest[:idx]
		}
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		switch typ {
		case "domain":
			rs.roots[value] = struct{}{}
		case "full":
			rs.fulls[value] = struct{}{}
		}
	}
	return rs, sc.Err()
}

// covered reports whether d is already matched by the rule set, honouring
// subdomain (`domain:`) semantics and exact (`full:`) semantics.
func (rs *ruleSet) covered(d string) bool {
	if _, ok := rs.fulls[d]; ok {
		return true
	}
	parts := strings.Split(d, ".")
	for i := range parts {
		if _, ok := rs.roots[strings.Join(parts[i:], ".")]; ok {
			return true
		}
	}
	return false
}

func (rs *ruleSet) merge(other *ruleSet) {
	for k := range other.roots {
		rs.roots[k] = struct{}{}
	}
	for k := range other.fulls {
		rs.fulls[k] = struct{}{}
	}
}

// validDomain mirrors upstream's validateDomainName so that anything we append
// is guaranteed to pass the upstream build.
func validDomain(d string) bool {
	if d == "" || len(d) > maxDomainLen {
		return false
	}
	for i := 0; i < len(d); i++ {
		c := d[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-' {
			continue
		}
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" || len(label) > maxLabelLen || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	return true
}

func isIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
		}
	}
	return true
}

func normalize(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(s, ".")
	if s == "" || isIPv4(s) || !validDomain(s) {
		return "", false
	}
	return s, true
}

// extract pulls domains out of a downloaded source according to its format.
func extract(format string, r io.Reader) ([]string, error) {
	seen := map[string]struct{}{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") || strings.HasPrefix(line, "[") {
			continue
		}
		var raw string
		switch format {
		case "dnsmasq":
			// server=/example.com/114.114.114.114
			if !strings.HasPrefix(line, "server=/") {
				continue
			}
			rest := strings.TrimPrefix(line, "server=/")
			idx := strings.Index(rest, "/")
			if idx < 0 {
				continue
			}
			raw = rest[:idx]
		case "abp":
			// ||example.com^
			if !strings.HasPrefix(line, "||") {
				continue
			}
			body := strings.TrimPrefix(line, "||")
			idx := strings.Index(body, "^")
			if idx < 0 || strings.TrimSpace(body[idx+1:]) != "" {
				continue
			}
			raw = body[:idx]
		case "hosts":
			// 127.0.0.1 example.com   /   0.0.0.0 example.com
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if fields[0] != "127.0.0.1" && fields[0] != "0.0.0.0" {
				continue
			}
			raw = fields[1]
		default:
			return nil, fmt.Errorf("unknown source format %q", format)
		}
		if d, ok := normalize(raw); ok {
			seen[d] = struct{}{}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	return out, nil
}

func fetch(url, rawPath string) (io.ReadCloser, error) {
	if rawPath != "" {
		if f, err := os.Open(rawPath); err == nil {
			return f, nil
		}
	}
	var lastErr error
	client := &http.Client{Timeout: 5 * time.Minute}
	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				return resp.Body, nil
			}
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
	return nil, fmt.Errorf("download failed: %w", lastErr)
}

type sourceReport struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	Raw      int    `json:"raw"`
	Accepted int    `json:"accepted_in_merge"`
}

type targetReport struct {
	BaseRules        int `json:"base_rules"`
	Added            int `json:"added"`
	SkippedCovered   int `json:"skipped_already_covered"`
	SkippedConflict  int `json:"skipped_conflict_with_exclude"`
	SkippedDuplicate int `json:"skipped_parent_child_duplicate"`
}

type buildInfo struct {
	GeneratedAt    string                  `json:"generated_at"`
	UpstreamCommit string                  `json:"upstream_commit,omitempty"`
	Sources        []sourceReport          `json:"sources"`
	Targets        map[string]targetReport `json:"targets"`
}

func main() {
	var (
		communityPath = flag.String("community", "./community", "path of the v2fly/domain-list-community checkout")
		basePath      = flag.String("base", "./base", "directory containing exported base lists (*.txt)")
		configPath    = flag.String("config", "./config/sources.json", "path of sources.json")
		reportPath    = flag.String("report", "./build-info.json", "path of the JSON report to write")
		rawDir        = flag.String("rawdir", "", "optional directory with pre-downloaded raw files named <source-name>.raw")
		upstreamSHA   = flag.String("upstream-sha", "", "upstream commit SHA recorded in the report")
		dryRun        = flag.Bool("dry-run", false, "do not append anything to the data directory")
	)
	flag.Parse()

	if err := run(*communityPath, *basePath, *configPath, *reportPath, *rawDir, *upstreamSHA, *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "[Fatal] %v\n", err)
		os.Exit(1)
	}
}

func run(communityPath, basePath, configPath, reportPath, rawDir, upstreamSHA string, dryRun bool) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	dataDir := filepath.Join(communityPath, "data")
	if _, err := os.Stat(dataDir); err != nil {
		return fmt.Errorf("data directory not found at %q: %w", dataDir, err)
	}

	// Load base rule sets (flattened + deduplicated by the upstream builder).
	baseSets := map[string]*ruleSet{}
	for target, t := range cfg.Targets {
		rs, err := parseExported(filepath.Join(basePath, t.BaseList+".txt"))
		if err != nil {
			return fmt.Errorf("load base list %q for target %q: %w", t.BaseList, target, err)
		}
		baseSets[target] = rs
	}
	excludeSets := map[string][]*ruleSet{}
	for target, t := range cfg.Targets {
		for _, name := range t.ExcludeLists {
			rs, err := parseExported(filepath.Join(basePath, name+".txt"))
			if err != nil {
				return fmt.Errorf("load exclude list %q for target %q: %w", name, target, err)
			}
			excludeSets[target] = append(excludeSets[target], rs)
		}
	}

	// Collect candidates per target.
	candidates := map[string]map[string]struct{}{}
	for _, t := range cfg.Targets {
		candidates[t.BaseList] = map[string]struct{}{}
	}
	info := buildInfo{GeneratedAt: time.Now().UTC().Format(time.RFC3339), UpstreamCommit: upstreamSHA}
	for _, src := range cfg.Sources {
		if _, ok := cfg.Targets[src.Target]; !ok {
			return fmt.Errorf("source %q targets unknown list %q", src.Name, src.Target)
		}
		rawFile := ""
		if rawDir != "" {
			rawFile = filepath.Join(rawDir, src.Name+".raw")
		}
		body, err := fetch(src.URL, rawFile)
		if err != nil {
			return fmt.Errorf("source %q: %w", src.Name, err)
		}
		domains, err := extract(src.Format, body)
		body.Close()
		if err != nil {
			return fmt.Errorf("source %q: %w", src.Name, err)
		}
		targetBase := cfg.Targets[src.Target].BaseList
		added := 0
		for _, d := range domains {
			if _, dup := candidates[targetBase][d]; dup {
				continue
			}
			candidates[targetBase][d] = struct{}{}
			added++
		}
		fmt.Printf("[source] %-18s format=%-8s target=%-18s raw=%d new=%d\n", src.Name, src.Format, src.Target, len(domains), added)
		info.Sources = append(info.Sources, sourceReport{Name: src.Name, Target: src.Target, Raw: len(domains), Accepted: added})
	}

	// Resolve each target: dedup among candidates, then filter against base + excludes.
	info.Targets = map[string]targetReport{}
	for target, t := range cfg.Targets {
		targetBase := t.BaseList
		cand := candidates[targetBase]
		base := baseSets[target]

		ordered := make([]string, 0, len(cand))
		for d := range cand {
			ordered = append(ordered, d)
		}
		sort.Slice(ordered, func(i, j int) bool {
			li, lj := strings.Count(ordered[i], "."), strings.Count(ordered[j], ".")
			if li != lj {
				return li < lj
			}
			return ordered[i] < ordered[j]
		})

		tr := targetReport{BaseRules: len(base.roots) + len(base.fulls)}
		kept := make([]string, 0, len(ordered))
		keptSet := newRuleSet()
		for _, d := range ordered {
			if base.covered(d) {
				tr.SkippedCovered++
				continue
			}
			conflict := false
			for _, ex := range excludeSets[target] {
				if ex.covered(d) {
					conflict = true
					break
				}
			}
			if conflict {
				tr.SkippedConflict++
				continue
			}
			if keptSet.covered(d) {
				tr.SkippedDuplicate++
				continue
			}
			keptSet.roots[d] = struct{}{}
			kept = append(kept, d)
		}
		tr.Added = len(kept)
		info.Targets[target] = tr
		fmt.Printf("[target] %-18s base=%-6d added=%-7d covered=%-6d conflict=%-4d dup=%d\n",
			targetBase, tr.BaseRules, tr.Added, tr.SkippedCovered, tr.SkippedConflict, tr.SkippedDuplicate)

		if dryRun || len(kept) == 0 {
			continue
		}
		sort.Strings(kept)
		if err := appendToData(filepath.Join(dataDir, targetBase), kept); err != nil {
			return fmt.Errorf("append to %q: %w", targetBase, err)
		}
	}

	if reportPath != "" {
		out, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(reportPath, append(out, '\n'), 0o644); err != nil {
			return err
		}
	}
	fmt.Println("[done] merge completed")
	return nil
}

func appendToData(path string, domains []string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	fmt.Fprintf(w, "\n# --- merged external domains (auto-generated, do not edit) ---\n")
	for _, d := range domains {
		fmt.Fprintf(w, "domain:%s\n", d)
	}
	return w.Flush()
}
