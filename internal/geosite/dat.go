// Package geosite decodes v2ray/xray GeoSite dat files without depending on
// v2ray-core. It centralizes the hand-rolled protobuf wire reader so the build
// tools share one interpretation of the schema.
package geosite

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// RuleType mirrors routercommon.Domain.Type.
type RuleType uint64

const (
	RuleKeyword RuleType = 0 // keyword: substring match
	RuleRegexp  RuleType = 1 // regular expression
	RuleDomain  RuleType = 2 // the value itself and all subdomains
	RuleFull    RuleType = 3 // exact match only
)

// Rule is one domain entry of a section.
type Rule struct {
	Type  RuleType
	Value string
	Attrs []string
}

// Section is one GeoSite section, keyed by its upper-case country code.
type Section struct {
	Name  string
	Rules []Rule
}

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
		if f.num <= 0 {
			return nil, fmt.Errorf("invalid field number %d", f.num)
		}
		switch f.wtyp {
		case 0:
			v, ni, err := readVarint(b, i)
			if err != nil {
				return nil, err
			}
			i = ni
			f.vi = v
		case 1:
			if i+8 > len(b) {
				return nil, errors.New("fixed64 field overflow")
			}
			i += 8
		case 2:
			l, ni, err := readVarint(b, i)
			if err != nil {
				return nil, err
			}
			i = ni
			if l > uint64(len(b)-i) {
				return nil, errors.New("length-delimited field overflow")
			}
			f.data = b[i : i+int(l)]
			i += int(l)
		case 5:
			if i+4 > len(b) {
				return nil, errors.New("fixed32 field overflow")
			}
			i += 4
		default:
			return nil, fmt.Errorf("unsupported wire type %d", f.wtyp)
		}
		out = append(out, f)
	}
	return out, nil
}

// ReadDat decodes a GeoSiteList and returns sections keyed by upper-case name.
func ReadDat(path string) (map[string]Section, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	top, err := parseFields(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}

	out := make(map[string]Section)
	for _, f := range top {
		if f.num != 1 || f.wtyp != 2 {
			continue
		}
		sec, err := decodeSection(f.data)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", path, err)
		}
		if sec.Name == "" {
			return nil, fmt.Errorf("parse %q: section without country code", path)
		}
		if _, dup := out[sec.Name]; dup {
			return nil, fmt.Errorf("parse %q: duplicate section %q", path, sec.Name)
		}
		out[sec.Name] = sec
	}
	return out, nil
}

func decodeSection(b []byte) (Section, error) {
	fields, err := parseFields(b)
	if err != nil {
		return Section{}, err
	}
	var sec Section
	for _, f := range fields {
		switch {
		case f.num == 1 && f.wtyp == 2:
			sec.Name = strings.ToUpper(string(f.data))
		case f.num == 2 && f.wtyp == 2:
			rule, err := decodeRule(f.data)
			if err != nil {
				return Section{}, fmt.Errorf("section %q: %w", sec.Name, err)
			}
			sec.Rules = append(sec.Rules, rule)
		}
	}
	return sec, nil
}

func decodeRule(b []byte) (Rule, error) {
	fields, err := parseFields(b)
	if err != nil {
		return Rule{}, err
	}
	var rule Rule
	for _, f := range fields {
		switch {
		case f.num == 1 && f.wtyp == 0:
			rule.Type = RuleType(f.vi)
		case f.num == 2 && f.wtyp == 2:
			rule.Value = string(f.data)
		case f.num == 3 && f.wtyp == 2:
			attr, err := decodeAttribute(f.data)
			if err != nil {
				return Rule{}, fmt.Errorf("rule %q: %w", rule.Value, err)
			}
			rule.Attrs = append(rule.Attrs, attr)
		}
	}
	return rule, nil
}

func decodeAttribute(b []byte) (string, error) {
	fields, err := parseFields(b)
	if err != nil {
		return "", err
	}
	var key string
	var hasBool bool
	var boolValue uint64
	for _, f := range fields {
		switch {
		case f.num == 1 && f.wtyp == 2:
			key = string(f.data)
		case f.num == 2 && f.wtyp == 0:
			hasBool = true
			boolValue = f.vi
		case f.num == 3 && f.wtyp == 0:
			return "", errors.New("attribute has an int value, which data syntax cannot represent")
		}
	}
	if key == "" {
		return "", errors.New("attribute without key")
	}
	if hasBool && boolValue == 0 {
		return "", fmt.Errorf("attribute %q is explicitly false, which data syntax cannot represent", key)
	}
	return key, nil
}
