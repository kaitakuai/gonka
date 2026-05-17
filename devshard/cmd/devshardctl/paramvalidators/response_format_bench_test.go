package paramvalidators

import (
	"strings"
	"testing"
)

// benchValidator returns the production-tuned validator. Reused across benchmarks so we
// measure the actual configuration that ships, not a synthetic one. Keep these constants
// in sync with the catalog entry in cmd/devshardctl/request_filters_parameters.go.
func benchValidator() ResponseFormatValidator {
	return ResponseFormatValidator{
		MaxDepth:      5,
		MaxSize:       16 * 1024,
		MaxNodes:      128,
		MaxBranch:     16,
		MaxEnum:       256,
		MaxNameLen:    64,
		MaxPatternLen: 512,
	}
}

func BenchmarkResponseFormatValidator_Absent(b *testing.B) {
	v := benchValidator()
	doc := parseDocument(b, `{"messages":[{"role":"user","content":"hello"}]}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := v.Validate(doc); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkResponseFormatValidator_TypeText(b *testing.B) {
	v := benchValidator()
	doc := parseDocument(b, `{"response_format":{"type":"text"},"messages":[{"role":"user","content":"hello"}]}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := v.Validate(doc); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkResponseFormatValidator_TypeJSONObject(b *testing.B) {
	v := benchValidator()
	doc := parseDocument(b, `{"response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hello"}]}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := v.Validate(doc); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkResponseFormatValidator_SimpleSchema(b *testing.B) {
	v := benchValidator()
	doc := parseDocument(b, `{"response_format":{"type":"json_schema","json_schema":{"name":"weather_v1","schema":{"type":"object","properties":{"city":{"type":"string"},"temp":{"type":"number"}},"required":["city"]}}},"messages":[{"role":"user","content":"hello"}]}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := v.Validate(doc); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResponseFormatValidator_AtLimits exercises the worst-case ACCEPTED schema: max depth, near max-nodes.
// A schema 5 levels deep with a few siblings at each level still fits under the 128-node cap.
func BenchmarkResponseFormatValidator_AtLimits(b *testing.B) {
	v := benchValidator()
	// 5 levels deep, 3 siblings per level: roughly 1 + 3 + 9 + 27 + 81 = 121 nodes <= 128.
	schema := buildLimitsSchema()
	body := `{"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":` + schema + `}},"messages":[{"role":"user","content":"hello"}]}`
	doc := parseDocument(b, body)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := v.Validate(doc); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResponseFormatValidator_RejectsRecursion measures the fast-reject path on the
// pathological case that motivated the validator. The depth check must fire early at depth 6.
func BenchmarkResponseFormatValidator_RejectsRecursion(b *testing.B) {
	v := benchValidator()
	deep := `{"type":"object"}`
	for i := 0; i < 200; i++ {
		deep = `{"type":"object","properties":{"x":` + deep + `}}`
	}
	body := `{"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":` + deep + `}},"messages":[{"role":"user","content":"hello"}]}`
	doc := parseDocument(b, body)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := v.Validate(doc); err == nil {
			b.Fatal("expected reject")
		}
	}
}

// BenchmarkResponseFormatValidator_RejectsOversized measures the size-gate path.
func BenchmarkResponseFormatValidator_RejectsOversized(b *testing.B) {
	v := benchValidator()
	big := `{"type":"object","properties":{"` + strings.Repeat("a", 17*1024) + `":{"type":"string"}}}`
	body := `{"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":` + big + `}},"messages":[{"role":"user","content":"hello"}]}`
	doc := parseDocument(b, body)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := v.Validate(doc); err == nil {
			b.Fatal("expected reject")
		}
	}
}

func buildLimitsSchema() string {
	// Recursively build a schema with 3 child properties at each level, 5 levels deep.
	var build func(depth int) string
	build = func(depth int) string {
		if depth <= 0 {
			return `{"type":"string"}`
		}
		child := build(depth - 1)
		return `{"type":"object","properties":{"a":` + child + `,"b":` + child + `,"c":` + child + `}}`
	}
	return build(4) // root + 4 nested = depth 5
}
