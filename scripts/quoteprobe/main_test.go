package main

import (
	"encoding/json"
	"testing"
	"time"
)

// The probe's file says which model, on which endpoint and when (Piano fase 6 chiusura, A7).
func TestProbeFileSaysTheModel(t *testing.T) {
	when := time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)
	data, err := probeFile("known34_1", "qwen-test", "http://engine.test/v1", when, []caseResult{{Case: "known-a", Asked: 3}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Round, Model, BaseURL, When string
		Cases                       []caseResult
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	for _, k := range []string{"round", "model", "base_url", "when", "cases"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("key %q missing: %s", k, data)
		}
	}
	var base string
	_ = json.Unmarshal(raw["base_url"], &base)
	if got.Round != "known34_1" || got.Model != "qwen-test" || base != "http://engine.test/v1" || got.When != "2026-11-16T09:30:00Z" {
		t.Errorf("header: %+v base %q", got, base)
	}
	if len(got.Cases) != 1 || got.Cases[0].Case != "known-a" || got.Cases[0].Asked != 3 {
		t.Errorf("cases: %+v", got.Cases)
	}
}
