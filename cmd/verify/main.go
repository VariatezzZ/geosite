// Command verify performs post-build health checks on the generated geosite.dat.
//
// It intentionally contains a tiny hand-rolled protobuf wire reader instead of
// depending on v2ray-core, so the build pipeline stays free of extra modules and
// the checks keep working even if the generator changes shape slightly.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	minSections  = 1500
	minCNDomains = 100000
	minAdsDomain = 190000
	minFileSize  = 3 * 1024 * 1024
	maxFileSize  = 15 * 1024 * 1024
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

type domainEntry struct {
	typ   uint64
	value string
}

// parseDat returns country_code -> domain entries.
func parseDat(path string) (map[string][]domainEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	top, err := parseFields(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	res := map[string][]domainEntry{}
	for _, f := range top {
		if f.num != 1 || f.wtyp != 2 {
			continue
		}
		sec, err := parseFields(f.data)
		if err != nil {
			return nil, err
		}
		var code string
		var doms []domainEntry
		for _, sf := range sec {
			switch {
			case sf.num == 1 && sf.wtyp == 2:
				code = string(sf.data)
			case sf.num == 2 && sf.wtyp == 2:
				df, err := parseFields(sf.data)
				if err != nil {
					return nil, err
				}
				var de domainEntry
				for _, x := range df {
					if x.num == 1 && x.wtyp == 0 {
						de.typ = x.vi
					}
					if x.num == 2 && x.wtyp == 2 {
						de.value = string(x.data)
					}
				}
				doms = append(doms, de)
			}
		}
		if code == "" {
			continue
		}
		res[strings.ToUpper(code)] = append(res[strings.ToUpper(code)], doms...)
	}
	return res, nil
}

func values(entries []domainEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.value)
	}
	sort.Strings(out)
	return out
}

func equalValues(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	)
	flag.Parse()

	if err := run(*datPath, *baseDatPath, *reportPath); err != nil {
		fmt.Fprintf(os.Stderr, "[Fatal] %v\n", err)
		os.Exit(1)
	}
}

func run(datPath, baseDatPath, reportPath string) error {
	rep := verifyReport{Passed: true}
	add := func(name string, passed bool, detail string) {
		rep.Checks = append(rep.Checks, checkResult{Name: name, Passed: passed, Detail: detail})
		if !passed {
			rep.Passed = false
		}
		fmt.Printf("[%s] %-34s %s\n", map[bool]string{true: " ok ", false: "FAIL"}[passed], name, detail)
	}

	st, err := os.Stat(datPath)
	if err != nil {
		return fmt.Errorf("stat %q: %w", datPath, err)
	}
	rep.FileSize = st.Size()
	add("file size in range", st.Size() >= minFileSize && st.Size() <= maxFileSize,
		fmt.Sprintf("%.2f MB", float64(st.Size())/1024/1024))

	sites, err := parseDat(datPath)
	if err != nil {
		return err
	}
	rep.Sections = len(sites)
	total := 0
	for _, d := range sites {
		total += len(d)
	}
	rep.TotalDomain = total
	add("section count", rep.Sections >= minSections, fmt.Sprintf("%d (min %d)", rep.Sections, minSections))
	add("total domain count", true, fmt.Sprintf("%d", total))

	cn, ok := sites["CN"]
	add("CN list present and large", ok && len(cn) >= minCNDomains, fmt.Sprintf("%d (min %d)", len(cn), minCNDomains))

	ads, ok := sites["CATEGORY-ADS-ALL"]
	add("CATEGORY-ADS-ALL present and large", ok && len(ads) >= minAdsDomain, fmt.Sprintf("%d (min %d)", len(ads), minAdsDomain))

	if baseDatPath != "" {
		base, err := parseDat(baseDatPath)
		if err != nil {
			return err
		}
		baseCN, _ := base["GEOLOCATION-!CN"]
		gotCN, _ := sites["GEOLOCATION-!CN"]
		same := equalValues(values(baseCN), values(gotCN))
		add("geolocation-!cn unchanged (base-wins policy)", same,
			fmt.Sprintf("base=%d final=%d", len(baseCN), len(gotCN)))
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
