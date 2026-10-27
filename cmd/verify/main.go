// Command verify performs post-build health checks on the generated geosite.dat.
//
// It uses the shared hand-rolled protobuf reader from internal/geosite instead
// of depending on v2ray-core, so the build pipeline stays free of extra modules
// and the checks keep working even if the generator changes shape slightly.
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

	"github.com/VariatezzZ/geosite/internal/geosite"
)

const (
	minSections  = 1500
	minCNDomains = 100000
	minAdsDomain = 190000
	minGFWList   = 2000
	minFileSize  = 3 * 1024 * 1024
	maxFileSize  = 15 * 1024 * 1024
)

func values(entries []geosite.Rule) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Value)
	}
	sort.Strings(out)
	return out
}

// uniqSorted returns the distinct values of a slice, sorted.
func uniqSorted(a []string) []string {
	set := make(map[string]struct{}, len(a))
	for _, x := range a {
		set[x] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

func toSet(a []string) map[string]struct{} {
	s := make(map[string]struct{}, len(a))
	for _, x := range a {
		s[x] = struct{}{}
	}
	return s
}

type checkResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type verifyReport struct {
	Passed      bool          `json:"passed"`
	Sections    int           `json:"sections"`
	TotalDomain int           `json:"total_domains"`
	FileSize    int64         `json:"file_size"`
	Checks      []checkResult `json:"checks"`
}

func main() {
	var (
		datPath     = flag.String("dat", "./publish/geosite.dat", "generated dat file to verify")
		baseDatPath = flag.String("basedat", "", "optional base dat file used for the geolocation-!cn invariant")
		reportPath  = flag.String("report", "", "optional path to write the verification report as JSON")
		expectFlag  = flag.String("expect-sections", "", "comma-separated sections expected in the generated dat (default: read verify/final-lists.txt next to basedat)")
		gfwFlag     = flag.String("gfwlist", "", "optional flattened gfwlist text (default: read verify/gfwlist.txt next to basedat)")
	)
	flag.Parse()

	if err := run(*datPath, *baseDatPath, *reportPath, *expectFlag, *gfwFlag); err != nil {
		fmt.Fprintf(os.Stderr, "[Fatal] %v\n", err)
		os.Exit(1)
	}
}

func run(datPath, baseDatPath, reportPath, expectFlag, gfwFlag string) error {
	rep := verifyReport{Passed: true}
	add := func(name string, passed bool, detail string) {
		rep.Checks = append(rep.Checks, checkResult{Name: name, Passed: passed, Detail: detail})
		if !passed {
			rep.Passed = false
		}
		fmt.Printf("[%s] %-34s %s\n", map[bool]string{true: " ok ", false: "FAIL"}[passed], name, detail)
	}

	expected, err := resolveExpectedSections(expectFlag, baseDatPath)
	if err != nil {
		return err
	}
	gfwPath, hasGFWFile := resolveAuxFile(gfwFlag, baseDatPath, "gfwlist.txt")

	st, err := os.Stat(datPath)
	if err != nil {
		return fmt.Errorf("stat %q: %w", datPath, err)
	}
	rep.FileSize = st.Size()
	add("file size in range", st.Size() >= minFileSize && st.Size() <= maxFileSize,
		fmt.Sprintf("%.2f MB", float64(st.Size())/1024/1024))

	sites, err := geosite.ReadDat(datPath)
	if err != nil {
		return err
	}
	rep.Sections = len(sites)
	total := 0
	for _, sec := range sites {
		total += len(sec.Rules)
	}
	rep.TotalDomain = total

	if len(expected) > 0 {
		got := make([]string, 0, len(sites))
		for name := range sites {
			got = append(got, name)
		}
		got = uniqSorted(got)
		missing, extra := diffSets(got, expected)
		add("section set matches final lists", len(missing) == 0 && len(extra) == 0,
			fmt.Sprintf("got=%s expected=%s missing=%s extra=%s",
				strings.Join(got, ","), strings.Join(expected, ","),
				listOrNone(missing), listOrNone(extra)))
	} else {
		add("section count", rep.Sections >= minSections, fmt.Sprintf("%d (min %d)", rep.Sections, minSections))
	}
	add("total domain count", true, fmt.Sprintf("%d", total))

	cn, ok := sites["CN"]
	add("CN list present and large", ok && len(cn.Rules) >= minCNDomains, fmt.Sprintf("%d (min %d)", len(cn.Rules), minCNDomains))

	ads, ok := sites["CATEGORY-ADS-ALL"]
	add("CATEGORY-ADS-ALL present and large", ok && len(ads.Rules) >= minAdsDomain, fmt.Sprintf("%d (min %d)", len(ads.Rules), minAdsDomain))

	// A finalized dat no longer carries a standalone GFWLIST section, so the
	// expected gfwlist values come from the flattened export written by merge.
	// Fall back to the section for legacy/full builds.
	var gfwVals []string
	gfwSource := ""
	if hasGFWFile {
		vals, err := parsePlainList(gfwPath)
		if err != nil {
			return err
		}
		gfwVals = uniqSorted(vals)
		gfwSource = "file"
		add("gfwlist source present and large", len(gfwVals) >= minGFWList,
			fmt.Sprintf("%d (min %d, source=file)", len(gfwVals), minGFWList))
	} else if section, ok := sites["GFWLIST"]; ok {
		gfwVals = uniqSorted(values(section.Rules))
		gfwSource = "section"
		add("GFWLIST present and large", len(gfwVals) >= minGFWList,
			fmt.Sprintf("%d (min %d)", len(gfwVals), minGFWList))
	}
	if baseDatPath != "" && len(expected) > 0 && len(gfwVals) == 0 {
		add("gfwlist verification source available", false,
			"final lists are configured but no gfwlist source was found")
	}

	if baseDatPath != "" && len(gfwVals) > 0 {
		base, err := geosite.ReadDat(baseDatPath)
		if err != nil {
			return err
		}
		// Invariant: gfwlist is folded into geolocation-!cn and nothing beyond
		// base + gfwlist leaks in. The upstream builder may additionally
		// collapse a few base entries covered by a gfwlist parent, so an exact
		// union equality is not expected; the meaningful checks are "no
		// leakage" and "gfwlist fully folded in".
		baseVals := uniqSorted(values(base["GEOLOCATION-!CN"].Rules))
		gotVals := uniqSorted(values(sites["GEOLOCATION-!CN"].Rules))

		baseSet, gfwSet, gotSet := toSet(baseVals), toSet(gfwVals), toSet(gotVals)
		leaked := 0
		for v := range gotSet {
			if _, inBase := baseSet[v]; !inBase {
				if _, inGfw := gfwSet[v]; !inGfw {
					leaked++
				}
			}
		}
		missingGfw := 0
		for v := range gfwSet {
			if _, ok := gotSet[v]; !ok {
				missingGfw++
			}
		}
		collapsed := 0
		for v := range baseSet {
			if _, ok := gotSet[v]; !ok {
				collapsed++
			}
		}
		add("geolocation-!cn == base + gfwlist", leaked == 0 && missingGfw == 0,
			fmt.Sprintf("base=%d gfwlist=%d final=%d leaked=%d missing_gfwlist=%d base_collapsed=%d source=%s",
				len(baseVals), len(gfwVals), len(gotVals), leaked, missingGfw, collapsed, gfwSource))
	}

	if len(sites) == 0 {
		add("non-empty output", false, "no sections found")
	}

	if reportPath != "" {
		out, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(reportPath, append(out, '\n'), 0o644); err != nil {
			return err
		}
	}
	if !rep.Passed {
		return errors.New("one or more health checks failed")
	}
	fmt.Println("[done] all health checks passed")
	return nil
}

// resolveExpectedSections returns the section names the dat is expected to
// contain. An explicit flag wins; otherwise the manifest written by merge is
// read from the verify subdirectory next to the base dat.
func resolveExpectedSections(explicit, baseDatPath string) ([]string, error) {
	if strings.TrimSpace(explicit) != "" {
		return parseCSVUpper(explicit), nil
	}
	if baseDatPath == "" {
		return nil, nil
	}
	p := filepath.Join(filepath.Dir(baseDatPath), "verify", "final-lists.txt")
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read final list manifest: %w", err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.ToUpper(line))
	}
	return uniqSorted(out), nil
}

// resolveAuxFile returns an explicit path, or falls back to a file under the
// verify subdirectory next to the base dat.
func resolveAuxFile(explicit, baseDatPath, name string) (string, bool) {
	if explicit != "" {
		return explicit, true
	}
	if baseDatPath == "" {
		return "", false
	}
	p := filepath.Join(filepath.Dir(baseDatPath), "verify", name)
	if _, err := os.Stat(p); err == nil {
		return p, true
	}
	return "", false
}

func parseCSVUpper(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, strings.ToUpper(part))
		}
	}
	return uniqSorted(out)
}

var (
	exportAttrSuffix = regexp.MustCompile(`:@[a-z0-9!]+(?:,@[a-z0-9!]+)*$`)
	spaceAttrSuffix  = regexp.MustCompile(`(?: @[a-z0-9!]+)+$`)
)

// parsePlainList reads a flattened list and returns the values of its rules,
// ignoring comments and attributes. Both the exporter syntax (":@a,@b") and
// the data input syntax (" @a @b") are accepted.
func parsePlainList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		typ, rest, ok := strings.Cut(line, ":")
		if !ok {
			typ, rest = "domain", line
		}
		switch typ {
		case "domain", "full", "keyword", "regexp":
		default:
			continue
		}
		if typ == "regexp" {
			rest = exportAttrSuffix.ReplaceAllString(rest, "")
			rest = spaceAttrSuffix.ReplaceAllString(rest, "")
		} else if idx := strings.IndexAny(rest, ": "); idx >= 0 {
			rest = rest[:idx]
		}
		rest = strings.ToLower(strings.TrimSpace(rest))
		if rest != "" {
			out = append(out, rest)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func diffSets(got, want []string) (missing, extra []string) {
	gotSet := toSet(got)
	wantSet := toSet(want)
	for _, x := range want {
		if _, ok := gotSet[x]; !ok {
			missing = append(missing, x)
		}
	}
	for _, x := range got {
		if _, ok := wantSet[x]; !ok {
			extra = append(extra, x)
		}
	}
	return missing, extra
}

func listOrNone(a []string) string {
	if len(a) == 0 {
		return "none"
	}
	return strings.Join(a, ",")
}
