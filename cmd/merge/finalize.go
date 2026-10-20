package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Attributes are exported as a trailing ":@a,@b" suffix, while the data input
// syntax expects space-separated attributes. This pattern matches the suffix
// without truncating colons that may appear inside a regexp value.
var exportedAttrSuffix = regexp.MustCompile(`:@[a-z0-9!]+(?:,[a-z0-9!]+)*$`)

// finalizeData replaces the upstream include structure with fully expanded
// copies of the configured final lists and removes every other data file.
//
// Each final list is rebuilt from two sources:
//   - the flattened base export produced by the upstream builder, converted
//     from its export syntax to the data input syntax so every rule type and
//     attribute is preserved;
//   - the plain domains accepted by this run for the matching target and for
//     any generated list folded into it via includes.
//
// Auxiliary files used by the verifier are written under a dedicated
// subdirectory next to the base exports, so they can never be mistaken for a
// base list on the next run.
func finalizeData(dataDir, basePath string, cfg Config, kept map[string][]string) error {
	verifyDir := filepath.Join(basePath, "verify")
	if err := os.MkdirAll(verifyDir, 0o755); err != nil {
		return fmt.Errorf("create verify directory: %w", err)
	}

	targetsByBase := map[string][]string{}
	for name, t := range cfg.Targets {
		targetsByBase[t.BaseList] = append(targetsByBase[t.BaseList], name)
	}

	finals := make([]flatList, 0, len(cfg.FinalLists))
	for _, name := range cfg.FinalLists {
		body, err := buildFinalList(basePath, name, targetsByBase, cfg, kept)
		if err != nil {
			return err
		}
		finals = append(finals, flatList{name: name, body: body})
	}

	for _, name := range cfg.VerifySources {
		if err := writeVerifySource(verifyDir, name, kept); err != nil {
			return err
		}
	}
	if err := writeFinalListManifest(verifyDir, cfg.FinalLists); err != nil {
		return err
	}

	// Everything is prepared in memory, so a failure above leaves data intact.
	if err := clearDataDir(dataDir); err != nil {
		return err
	}
	for _, f := range finals {
		if err := os.WriteFile(filepath.Join(dataDir, f.name), f.body, 0o644); err != nil {
			return fmt.Errorf("write final list %q: %w", f.name, err)
		}
		fmt.Printf("[final ] %-18s rules=%d\n", f.name, countRuleLines(f.body))
	}
	return nil
}

type flatList struct {
	name string
	body []byte
}

func buildFinalList(basePath, name string, targetsByBase map[string][]string, cfg Config, kept map[string][]string) ([]byte, error) {
	if !validListName(name) {
		return nil, fmt.Errorf("invalid final list name %q", name)
	}

	var buf bytes.Buffer
	baseFile := filepath.Join(basePath, name+".txt")
	if data, err := os.ReadFile(baseFile); err == nil {
		if len(data) > 0 {
			if err := writeExportedList(&buf, data); err != nil {
				return nil, fmt.Errorf("normalize base list %q: %w", name, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read base list %q: %w", name, err)
	} else if t, ok := cfg.Targets[name]; !ok || !t.BaseOptional {
		return nil, fmt.Errorf("base list %q not found under the base directory", name)
	}

	seen := map[string]struct{}{}
	appendDomains := func(label string, domains []string) {
		if len(domains) == 0 {
			return
		}
		fmt.Fprintf(&buf, "# --- %s (auto-generated) ---\n", label)
		for _, d := range domains {
			if _, dup := seen[d]; dup {
				continue
			}
			seen[d] = struct{}{}
			fmt.Fprintf(&buf, "domain:%s\n", d)
		}
	}

	sources := append([]string(nil), targetsByBase[name]...)
	sort.Strings(sources)
	for _, target := range sources {
		appendDomains(target, kept[target])
	}

	includes := append([]string(nil), cfg.Includes[name]...)
	sort.Strings(includes)
	for _, inc := range includes {
		domains, ok := kept[inc]
		if !ok {
			return nil, fmt.Errorf("final list %q includes unknown generated list %q", name, inc)
		}
		appendDomains(inc, domains)
	}

	if buf.Len() == 0 {
		return nil, fmt.Errorf("final list %q is empty", name)
	}
	return buf.Bytes(), nil
}

// writeVerifySource exports a plain list used by the verifier. Only generated
// targets are accepted, so the file always reflects this run's accepted domains.
func writeVerifySource(verifyDir, name string, kept map[string][]string) error {
	domains, ok := kept[name]
	if !ok {
		return fmt.Errorf("verify source %q is not a generated target", name)
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# --- %s (auto-generated) ---\n", name)
	for _, d := range domains {
		fmt.Fprintf(&buf, "domain:%s\n", d)
	}
	if err := os.WriteFile(filepath.Join(verifyDir, name+".txt"), buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write verify source %q: %w", name, err)
	}
	return nil
}

func writeFinalListManifest(basePath string, names []string) error {
	var buf bytes.Buffer
	for _, name := range names {
		fmt.Fprintf(&buf, "%s\n", name)
	}
	if err := os.WriteFile(filepath.Join(basePath, "final-lists.txt"), buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write final list manifest: %w", err)
	}
	return nil
}

func clearDataDir(dataDir string) error {
	clean := filepath.Clean(dataDir)
	if filepath.Base(clean) != "data" {
		return fmt.Errorf("refusing to clear unexpected directory %q", dataDir)
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		return fmt.Errorf("read data directory: %w", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(clean, entry.Name())); err != nil {
			return fmt.Errorf("remove data entry %q: %w", entry.Name(), err)
		}
	}
	fmt.Printf("[final ] removed %d data entries\n", len(entries))
	return nil
}

func validateFinalConfig(cfg Config) error {
	if len(cfg.FinalLists) == 0 {
		return errors.New("final_lists must not be empty")
	}

	seen := map[string]struct{}{}
	for _, name := range cfg.FinalLists {
		if !validListName(name) {
			return fmt.Errorf("invalid final list name %q", name)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("duplicate final list %q", name)
		}
		seen[name] = struct{}{}
	}
	for _, name := range cfg.VerifySources {
		if !validListName(name) {
			return fmt.Errorf("invalid verify source name %q", name)
		}
		if _, ok := cfg.Targets[name]; !ok {
			return fmt.Errorf("verify source %q is not a generated target", name)
		}
		if _, overlap := seen[name]; overlap {
			return fmt.Errorf("verify source %q is also a final list", name)
		}
	}
	return nil
}

// writeExportedList converts a flattened export into data input syntax. The
// upstream exporter writes attributes as ":@a,@b", while data files expect
// separate " @a @b" fields.
func writeExportedList(dst *bytes.Buffer, data []byte) error {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		normalized, err := normalizeExportedLine(line)
		if err != nil {
			return err
		}
		dst.WriteString(normalized)
		dst.WriteByte('\n')
	}
	return sc.Err()
}

func normalizeExportedLine(line string) (string, error) {
	typ, rest, ok := strings.Cut(line, ":")
	if !ok {
		typ, rest = "domain", line
	}
	switch typ {
	case "domain", "full", "keyword", "regexp":
	default:
		return "", fmt.Errorf("unsupported exported rule type %q", typ)
	}

	attrs := ""
	if loc := exportedAttrSuffix.FindStringIndex(rest); loc != nil {
		attrs = rest[loc[0]+1:] // drop the leading ':'
		rest = rest[:loc[0]]
	}
	if rest == "" {
		return "", fmt.Errorf("empty value in exported line %q", line)
	}

	out := typ + ":" + rest
	if attrs != "" {
		out += " " + strings.ReplaceAll(attrs, ",", " ")
	}
	return out, nil
}

func validListName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '!' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func countRuleLines(b []byte) int {
	n := 0
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		n++
	}
	return n
}
