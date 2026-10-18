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
	Name      string `json:"name"`
	URL       string `json:"url"`
	Format    string `json:"format"`
	Target    string `json:"target"`
	Reference bool   `json:"reference"`
}

type Config struct {
	Targets  map[string]Target   `json:"targets"`
	Includes map[string][]string `json:"includes"`
	Sources  []Source            `json:"sources"`
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
		case "clash":
			//   - 'example.com'   /   - '+.example.com'
			f := strings.TrimSpace(line)
			if !strings.HasPrefix(f, "-") {
				continue
			}
			f = strings.TrimSpace(strings.TrimPrefix(f, "-"))
			f = strings.Trim(f, "'\"")
			raw = strings.TrimPrefix(f, "+.")
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

// refReport records how one target's candidate set relates to a reference list
// (e.g. accelerated candidates vs ChinaMax).
type refReport struct {
	Name          string `json:"name"`
	Size          int    `json:"size"`
	CandidatesIn  int    `json:"candidates_in_ref"`
	CandidatesOut int    `json:"candidates_not_in_ref"`
	KeptIn        int    `json:"kept_in_ref"`
	KeptOut       int    `json:"kept_not_in_ref"`
}

type comparisonReport struct {
	Target string      `json:"target"`
	Refs   []refReport `json:"refs"`
}

type referenceReport struct {
	Name    string `json:"name"`
	Domains int    `json:"domains"`
}

type buildInfo struct {
	GeneratedAt    string                  `json:"generated_at"`
	UpstreamCommit string                  `json:"upstream_commit,omitempty"`
	Sources        []sourceReport          `json:"sources"`
	References     []referenceReport       `json:"references,omitempty"`
	Targets        map[string]targetReport `json:"targets"`
	Comparisons    []comparisonReport      `json:"comparisons,omitempty"`
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
	// A target marked base_optional (e.g. our own gfwlist, which upstream does
	// not provide) starts from an empty base when the file is absent.
	baseSets := map[string]*ruleSet{}
	for target, t := range cfg.Targets {
		p := filepath.Join(basePath, t.BaseList+".txt")
		if _, err := os.Stat(p); err != nil {
			if t.BaseOptional {
				baseSets[target] = newRuleSet()
				continue
			}
			return fmt.Errorf("load base list %q for target %q: %w", t.BaseList, target, err)
		}
		rs, err := parseExported(p)
		if err != nil {
			return fmt.Errorf("load base list %q for target %q: %w", t.BaseList, target, err)
		}
		baseSets[target] = rs
	}

	// Base exclude sets, loaded once per distinct list name. Generated lists
	// (via the includes map) are merged in later, at resolve time.
	baseExcludes := map[string]*ruleSet{}
	loadExclude := func(name string) error {
		if _, ok := baseExcludes[name]; ok {
			return nil
		}
		rs, err := parseExported(filepath.Join(basePath, name+".txt"))
		if err != nil {
			return err
		}
		baseExcludes[name] = rs
		return nil
	}
	for _, t := range cfg.Targets {
		for _, name := range t.ExcludeLists {
			if err := loadExclude(name); err != nil {
				return fmt.Errorf("load exclude list %q: %w", name, err)
			}
		}
	}

	// Collect candidates per target; reference sources yield comparison sets only.
	candidates := map[string]map[string]struct{}{}
	for _, t := range cfg.Targets {
		candidates[t.BaseList] = map[string]struct{}{}
	}
	references := map[string]*ruleSet{}

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

		if src.Reference {
			rs := newRuleSet()
			for _, d := range domains {
				rs.roots[d] = struct{}{}
			}
			references[src.Name] = rs
			info.References = append(info.References, referenceReport{Name: src.Name, Domains: len(domains)})
			fmt.Printf("[ref   ] %-18s format=%-8s domains=%d\n", src.Name, src.Format, len(domains))
			continue
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

		if len(references) > 0 && len(cand) > 0 {
			info.Comparisons = append(info.Comparisons, compareWithRefs(targetBase, cand, kept, references))
		}

		if dryRun || len(kept) == 0 {
			continue
		}
		sort.Strings(kept)
		if err := appendToData(filepath.Join(dataDir, targetBase), kept); err != nil {
			return fmt.Errorf("append to %q: %w", targetBase, err)
		}
	}

	// Fold generated lists into their containers via include: directives
	// (e.g. geolocation-!cn gets "include:gfwlist").
	if !dryRun {
		containers := make([]string, 0, len(cfg.Includes))
		for c := range cfg.Includes {
			containers = append(containers, c)
		}
		sort.Strings(containers)
		for _, container := range containers {
			if err := appendIncludes(filepath.Join(dataDir, container), cfg.Includes[container]); err != nil {
				return fmt.Errorf("append include to %q: %w", container, err)
			}
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
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
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

// appendIncludes appends "include:<name>" directives to a container data file.
func appendIncludes(path string, includes []string) error {
	if len(includes) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	fmt.Fprintf(w, "\n# --- includes merged by geosite builder (auto-generated, do not edit) ---\n")
	for _, inc := range includes {
		fmt.Fprintf(w, "include:%s\n", inc)
	}
	return w.Flush()
}

func cloneRuleSet(rs *ruleSet) *ruleSet {
	out := newRuleSet()
	for k := range rs.roots {
		out.roots[k] = struct{}{}
	}
	for k := range rs.fulls {
		out.fulls[k] = struct{}{}
	}
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

// compareWithRefs reports how a target's candidates and kept set split against
// each reference list (e.g. ChinaMax).
func compareWithRefs(target string, cand map[string]struct{}, kept []string, refs map[string]*ruleSet) comparisonReport {
	cr := comparisonReport{Target: target}
	for name, ref := range refs {
		rr := refReport{Name: name, Size: len(ref.roots)}
		for d := range cand {
			if ref.covered(d) {
				rr.CandidatesIn++
			} else {
				rr.CandidatesOut++
			}
		}
		for _, d := range kept {
			if ref.covered(d) {
				rr.KeptIn++
			} else {
				rr.KeptOut++
			}
		}
		cr.Refs = append(cr.Refs, rr)
	}
	sort.Slice(cr.Refs, func(i, j int) bool { return cr.Refs[i].Name < cr.Refs[j].Name })
	return cr
}
