// Command merge merges external domain lists (China domains, ad domains) into a
// freshly checked out v2fly/domain-list-community data directory.
//
// Design notes:
//   - The base data is NOT re-parsed here. Instead we decode the flattened,
//     already-deduplicated GeoSite dat produced by the upstream builder
//     (`go run ./ --outputdir=... --outputname=base.dat`). This reuses
//     upstream's include resolution, attribute filtering, full/domain
//     precedence and parent-domain de-duplication.
//   - Every candidate domain is skipped when it is already covered by the base
//     list, and for the "cn" target also when it is covered by geolocation-!cn
//     (conflict policy "base wins": such domains stay in the proxy list and are
//     never added to cn).
//   - Only plain `domain:` rules are emitted, so anything we add stays compatible
//     with upstream's syntax validation.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/VariatezzZ/geosite/internal/geosite"
)

const (
	maxDomainLen = 253
	maxLabelLen  = 63
	userAgent    = "geosite-builder/1.0"
)

type Target struct {
	BaseList     string   `json:"base_list"`
	BaseOptional bool     `json:"base_optional"`
	ExcludeLists []string `json:"exclude_lists"`
}

type Source struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Format string `json:"format"`
	Target string `json:"target"`
}

type Config struct {
	Targets       map[string]Target   `json:"targets"`
	Includes      map[string][]string `json:"includes"`
	FinalLists    []string            `json:"final_lists"`
	VerifySources []string            `json:"verify_sources"`
	Sources       []Source            `json:"sources"`
}

// ruleSet is the flattened view of one base list.
type ruleSet struct {
	roots map[string]struct{} // `domain:` values (match the value itself and all subdomains)
	fulls map[string]struct{} // `full:` values (exact match only)
}

func newRuleSet() *ruleSet {
	return &ruleSet{roots: map[string]struct{}{}, fulls: map[string]struct{}{}}
}

// rulesFromSection extracts the coverage rules of one base section. Keyword and
// regexp rules are intentionally ignored, matching the previous text-based
// parser: coverage checks only need domain and full semantics.
func rulesFromSection(sec geosite.Section) *ruleSet {
	rs := newRuleSet()
	for _, r := range sec.Rules {
		value := strings.ToLower(strings.TrimSpace(r.Value))
		if value == "" {
			continue
		}
		switch r.Type {
		case geosite.RuleDomain:
			rs.roots[value] = struct{}{}
		case geosite.RuleFull:
			rs.fulls[value] = struct{}{}
		}
	}
	return rs
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
	if len(d) > maxDomainLen || !validRuleChars(d) {
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
	// 裸 TLD（如 dnsmasq 的 server=/top/114.114.114.114、hosts 的 0.0.0.0 localhost）
	// 展开后是 domain:top / domain:localhost，会让整个后缀命中目标清单、误伤境外站点，
	// 故外部源中的裸 TLD 一律不添加；整 TLD 的归属只由基底 tld-cn / tld-!cn 决定。
	if s == "" || !strings.Contains(s, ".") || isIPv4(s) || !validDomain(s) {
		return "", false
	}
	return s, true
}

// extract pulls domains out of a downloaded source according to its format.
func extract(format string, r io.Reader) ([]string, error) {
	seen := map[string]struct{}{}

	if format == "gfwlist" {
		body, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		decoded, err := decodeBase64Text(body)
		if err != nil {
			return nil, fmt.Errorf("gfwlist base64: %w", err)
		}
		for _, line := range strings.Split(decoded, "\n") {
			if tok, ok := autoproxyToken(line); ok {
				if d, ok := normalize(tok); ok {
					seen[d] = struct{}{}
				}
			}
		}
		return mapKeys(seen), nil
	}

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
	return mapKeys(seen), nil
}

// mapKeys returns the keys of a set as an unsorted slice.
func mapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// decodeBase64Text strips all whitespace, then base64-decodes the whole input.
// The GFWList file is a base64-encoded AutoProxy list.
func decodeBase64Text(b []byte) (string, error) {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		if c == '\n' || c == '\r' || c == ' ' || c == '\t' {
			continue
		}
		sb.WriteByte(c)
	}
	dec, err := base64.StdEncoding.DecodeString(sb.String())
	if err != nil {
		return "", err
	}
	return string(dec), nil
}

// autoproxyToken derives a host from one AutoProxy/GFWList rule line.
//
// Following this project's "take as much as possible" policy for GFWList, it
// keeps hosts derived from path-scoped and @@ whitelist rules too, and drops
// only regex rules (no single host is recoverable from them).
func autoproxyToken(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "!") || strings.HasPrefix(s, "[") {
		return "", false
	}
	if strings.HasPrefix(s, "@@") {
		s = s[2:]
	}
	if strings.HasPrefix(s, "/") && strings.HasSuffix(s, "/") {
		return "", false // regex rule
	}
	if strings.HasPrefix(s, "||") {
		s = s[2:]
	} else {
		s = strings.TrimLeft(s, "|.")
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "^/*?|="); i >= 0 {
		s = s[:i]
	}
	s = strings.Trim(s, ".")
	return s, s != ""
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
	GeneratedAt      string                  `json:"generated_at"`
	UpstreamCommit   string                  `json:"upstream_commit,omitempty"`
	Sources          []sourceReport          `json:"sources"`
	Targets          map[string]targetReport `json:"targets"`
	FinalLists       []string                `json:"final_lists,omitempty"`
	PackagedSections int                     `json:"packaged_sections,omitempty"`
}

func main() {
	var (
		communityPath = flag.String("community", "./community", "path of the v2fly/domain-list-community checkout")
		basePath      = flag.String("base", "./base", "directory containing the exported base.dat")
		configPath    = flag.String("config", "./config/sources.json", "path of sources.json")
		reportPath    = flag.String("report", "./build-info.json", "path of the JSON report to write")
		rawDir        = flag.String("rawdir", "", "optional directory with pre-downloaded raw files named <source-name>.raw")
		upstreamSHA   = flag.String("upstream-sha", "", "upstream commit SHA recorded in the report")
		dryRun        = flag.Bool("dry-run", false, "do not write anything to the data directory")
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
	if err := validateFinalConfig(cfg); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	dataDir := filepath.Join(communityPath, "data")
	if _, err := os.Stat(dataDir); err != nil {
		return fmt.Errorf("data directory not found at %q: %w", dataDir, err)
	}

	// Load the flattened base data once. The upstream builder produces a
	// GeoSite dat file, which is a far more stable contract than its plaintext
	// export format.
	baseDatPath := filepath.Join(basePath, "base.dat")
	baseDat, err := geosite.ReadDat(baseDatPath)
	if err != nil {
		return fmt.Errorf("load base dat %q: %w", baseDatPath, err)
	}

	baseSets := map[string]*ruleSet{}
	for target, t := range cfg.Targets {
		sec, ok := baseDat[strings.ToUpper(t.BaseList)]
		if !ok {
			if t.BaseOptional {
				baseSets[target] = newRuleSet()
				continue
			}
			return fmt.Errorf("base section %q for target %q not found in %q", t.BaseList, target, baseDatPath)
		}
		baseSets[target] = rulesFromSection(sec)
	}

	// Base exclude sets, loaded once per distinct list name. Generated lists
	// (via the includes map) are merged in later, at resolve time.
	baseExcludes := map[string]*ruleSet{}
	loadExclude := func(name string) error {
		if _, ok := baseExcludes[name]; ok {
			return nil
		}
		sec, ok := baseDat[strings.ToUpper(name)]
		if !ok {
			return fmt.Errorf("exclude section %q not found in %q", name, baseDatPath)
		}
		baseExcludes[name] = rulesFromSection(sec)
		return nil
	}
	for _, t := range cfg.Targets {
		for _, name := range t.ExcludeLists {
			if err := loadExclude(name); err != nil {
				return fmt.Errorf("load exclude list %q: %w", name, err)
			}
		}
	}

	// Collect candidates per target.
	candidates := map[string]map[string]struct{}{}
	for _, t := range cfg.Targets {
		candidates[t.BaseList] = map[string]struct{}{}
	}

	info := buildInfo{GeneratedAt: time.Now().UTC().Format(time.RFC3339), UpstreamCommit: upstreamSHA}
	for _, src := range cfg.Sources {
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

		if _, ok := cfg.Targets[src.Target]; !ok {
			return fmt.Errorf("source %q targets unknown list %q", src.Name, src.Target)
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

	// Resolve targets in dependency order. A target that excludes a list L must
	// wait until every list that L "includes" has been generated, because a
	// generated list (gfwlist) is folded into L's effective exclude set here —
	// this is what makes the "cn compares against the expanded geolocation-!cn"
	// rule work without a separate gfwlist comparison.
	order, err := resolveOrder(cfg)
	if err != nil {
		return err
	}

	info.Targets = map[string]targetReport{}
	generated := map[string]*ruleSet{}
	keptDomains := map[string][]string{}
	for _, target := range order {
		t := cfg.Targets[target]
		targetBase := t.BaseList
		cand := candidates[targetBase]
		base := baseSets[target]

		// Effective excludes = base file for the name + any generated list it includes.
		var excludes []*ruleSet
		for _, name := range t.ExcludeLists {
			rs := cloneRuleSet(baseExcludes[name])
			for _, inc := range cfg.Includes[name] {
				if g, ok := generated[inc]; ok {
					rs.merge(g)
				}
			}
			excludes = append(excludes, rs)
		}

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
			for _, ex := range excludes {
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
		generated[target] = keptSet
		fmt.Printf("[target] %-18s base=%-6d added=%-7d covered=%-6d conflict=%-4d dup=%d\n",
			targetBase, tr.BaseRules, tr.Added, tr.SkippedCovered, tr.SkippedConflict, tr.SkippedDuplicate)

		sort.Strings(kept)
		keptDomains[target] = kept
	}

	if !dryRun {
		// Replace the upstream include structure with fully expanded copies of
		// the configured final lists, then drop every other data file.
		if err := finalizeData(dataDir, basePath, baseDat, cfg, keptDomains); err != nil {
			return err
		}
		info.FinalLists = append([]string(nil), cfg.FinalLists...)
		info.PackagedSections = len(cfg.FinalLists)
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

func cloneRuleSet(rs *ruleSet) *ruleSet {
	out := newRuleSet()
	out.merge(rs)
	return out
}

// resolveOrder returns target names topologically sorted so that any list a
// target's exclude_lists "includes" is generated first.
func resolveOrder(cfg Config) ([]string, error) {
	names := make([]string, 0, len(cfg.Targets))
	for n := range cfg.Targets {
		names = append(names, n)
	}
	sort.Strings(names)

	deps := map[string]map[string]struct{}{}
	for n, t := range cfg.Targets {
		set := map[string]struct{}{}
		for _, ex := range t.ExcludeLists {
			for _, inc := range cfg.Includes[ex] {
				// Skip self: a list that excludes its own container (gfwlist
				// excludes geolocation-!cn, which includes gfwlist) still only
				// needs the pure base for that exclusion.
				if inc == n {
					continue
				}
				if _, ok := cfg.Targets[inc]; ok {
					set[inc] = struct{}{}
				}
			}
		}
		deps[n] = set
	}

	var order []string
	done := map[string]bool{}
	for len(order) < len(names) {
		progress := false
		for _, n := range names {
			if done[n] {
				continue
			}
			ready := true
			for d := range deps[n] {
				if !done[d] {
					ready = false
					break
				}
			}
			if ready {
				order = append(order, n)
				done[n] = true
				progress = true
			}
		}
		if !progress {
			return nil, errors.New("circular include/exclude dependency among targets")
		}
	}
	return order, nil
}
