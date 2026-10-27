package geosite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func testBytesField(num int, data []byte) []byte {
	out := testVarint(nil, uint64(num<<3|2))
	out = testVarint(out, uint64(len(data)))
	return append(out, data...)
}

func testVarintField(num int, v uint64) []byte {
	out := testVarint(nil, uint64(num<<3|0))
	return testVarint(out, v)
}

func testBoolAttr(key string, value uint64) []byte {
	out := testBytesField(1, []byte(key))
	return append(out, testVarintField(2, value)...)
}

func testRule(typ uint64, value string, attrs ...[]byte) []byte {
	out := testVarintField(1, typ)
	out = append(out, testBytesField(2, []byte(value))...)
	for _, attr := range attrs {
		out = append(out, testBytesField(3, attr)...)
	}
	return out
}

func testSection(code string, rules ...[]byte) []byte {
	out := testBytesField(1, []byte(code))
	for _, rule := range rules {
		out = append(out, testBytesField(2, rule)...)
	}
	return out
}

func writeTestDat(t *testing.T, sections ...[]byte) string {
	t.Helper()
	var raw []byte
	for _, sec := range sections {
		raw = append(raw, testBytesField(1, sec)...)
	}
	path := filepath.Join(t.TempDir(), "base.dat")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadDat(t *testing.T) {
	path := writeTestDat(t, testSection("CATEGORY-ADS-ALL",
		testRule(uint64(RuleDomain), "adash-c.m.taobao.com", testBoolAttr("ads", 1), testBoolAttr("telemetry", 1)),
		testRule(uint64(RuleFull), "exact.example.com"),
		testRule(uint64(RuleRegexp), `^ads\.example\.com$`),
		testRule(uint64(RuleKeyword), "-ads-"),
	))

	got, err := ReadDat(path)
	if err != nil {
		t.Fatalf("ReadDat() error: %v", err)
	}
	sec, ok := got["CATEGORY-ADS-ALL"]
	if !ok {
		t.Fatalf("section CATEGORY-ADS-ALL not found, got %v", got)
	}
	if len(sec.Rules) != 4 {
		t.Fatalf("got %d rules, want 4", len(sec.Rules))
	}

	first := sec.Rules[0]
	if first.Type != RuleDomain || first.Value != "adash-c.m.taobao.com" {
		t.Fatalf("unexpected first rule: %+v", first)
	}
	if strings.Join(first.Attrs, ",") != "ads,telemetry" {
		t.Fatalf("got attrs %v, want [ads telemetry]", first.Attrs)
	}
	if sec.Rules[1].Type != RuleFull || sec.Rules[1].Value != "exact.example.com" {
		t.Fatalf("unexpected full rule: %+v", sec.Rules[1])
	}
	if sec.Rules[2].Type != RuleRegexp || sec.Rules[2].Value != `^ads\.example\.com$` {
		t.Fatalf("unexpected regexp rule: %+v", sec.Rules[2])
	}
	if sec.Rules[3].Type != RuleKeyword || sec.Rules[3].Value != "-ads-" {
		t.Fatalf("unexpected keyword rule: %+v", sec.Rules[3])
	}
}

func TestReadDatRejectsIntAttribute(t *testing.T) {
	intAttr := testBytesField(1, []byte("ads"))
	intAttr = append(intAttr, testVarintField(3, 1)...)
	path := writeTestDat(t, testSection("CN", testRule(uint64(RuleDomain), "example.com", intAttr)))

	if _, err := ReadDat(path); err == nil {
		t.Fatal("ReadDat() succeeded, want error for int-valued attribute")
	}
}

func TestReadDatRejectsDuplicateSection(t *testing.T) {
	path := writeTestDat(t,
		testSection("CN", testRule(uint64(RuleDomain), "a.example")),
		testSection("CN", testRule(uint64(RuleDomain), "b.example")),
	)

	if _, err := ReadDat(path); err == nil {
		t.Fatal("ReadDat() succeeded, want error for duplicate section")
	}
}
