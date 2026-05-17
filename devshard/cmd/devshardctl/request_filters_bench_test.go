package main

import (
	"testing"
)

// Pre-baked request bodies for the end-to-end pipeline benchmark. Bytes are reused across
// b.N iterations because normalizeChatRequest does not mutate the input.
var (
	benchBodyMinimal = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)

	benchBodyTypical = []byte(`{"model":"moonshotai/Kimi-K2.6","messages":[{"role":"user","content":"hello"}],"temperature":0.7,"top_p":0.95,"max_tokens":512}`)

	benchBodyHeavy = []byte(`{
		"model":"moonshotai/Kimi-K2.6",
		"messages":[
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"What is the weather in Berlin?"}
		],
		"temperature":0.7,
		"top_p":0.95,
		"top_k":40,
		"min_p":0.05,
		"repetition_penalty":1.05,
		"max_tokens":512,
		"stop":["\n\n","STOP"],
		"seed":42,
		"n":1,
		"thinking":{"type":"enabled"},
		"tools":[
			{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}
		]
	}`)

	benchBodyWithResponseFormat = []byte(`{
		"model":"moonshotai/Kimi-K2.6",
		"messages":[{"role":"user","content":"hello"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"weather_v1","schema":{"type":"object","properties":{"city":{"type":"string"},"temp":{"type":"number"}},"required":["city"]}}}
	}`)

	benchBodyRejectedUnknown = []byte(`{"messages":[{"role":"user","content":"hello"}],"frequency_penalty":1.5}`)

	benchBodyRejectedRecursive = buildBenchPathologicalBody()
)

func buildBenchPathologicalBody() []byte {
	deep := `{"type":"object"}`
	for i := 0; i < 200; i++ {
		deep = `{"type":"object","properties":{"x":` + deep + `}}`
	}
	return []byte(`{"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":` + deep + `}},"messages":[{"role":"user","content":"hello"}]}`)
}

func BenchmarkNormalizeChatRequest(b *testing.B) {
	cases := []struct {
		name string
		body []byte
	}{
		{"Minimal", benchBodyMinimal},
		{"Typical", benchBodyTypical},
		{"Heavy", benchBodyHeavy},
		{"WithResponseFormat", benchBodyWithResponseFormat},
		{"RejectedUnknownField", benchBodyRejectedUnknown},
		{"RejectedRecursiveSchema", benchBodyRejectedRecursive},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.body)))
			for i := 0; i < b.N; i++ {
				_, _, _ = normalizeChatRequest(tc.body)
			}
		})
	}
}
