package main

import (
	"strings"
	"testing"
)

func TestSupersedeTraceDistinguishesCandidateGenerationFromClassifier(t *testing.T) {
	const newer, older = "aabbccdd11223344", "556677889900aabb"
	res := &Result{SupersedeTrace: []SupersedeTrace{
		{NewerID: "11111111", OlderID: "22222222", Status: "proposed", Detail: "neither"},
		{NewerID: "aabbccdd", OlderID: "55667788", Status: "not proposed",
			Detail: "below the cosine floor and no lexical replacement"},
	}}
	if got := supersedeTraceDetail(res, newer, older); !strings.Contains(got, "candidate scan saw and rejected") {
		t.Errorf("trace detail = %q, want the candidate-generation diagnosis", got)
	}
	res.SupersedeTrace[1] = SupersedeTrace{NewerID: "aabbccdd", OlderID: "55667788", Status: "proposed", Detail: "neither"}
	if got := supersedeTraceDetail(res, newer, older); !strings.Contains(got, "classifier rejected") {
		t.Errorf("trace detail = %q, want the classifier diagnosis", got)
	}
	if got := supersedeTraceDetail(&Result{}, newer, older); got != "" {
		t.Errorf("trace detail with no capture = %q, want empty", got)
	}
}

func TestParseSupersedeTrace(t *testing.T) {
	out := "  TRACE aaaaaaaa -> cccccccc not proposed: below the cosine floor\n" +
		"  TRACE eeeeeeee -> 11111111 proposed: classifier verdict neither\n" +
		"other report text\n"
	got := parseSupersedeTrace(out)
	if len(got) != 2 {
		t.Fatalf("parsed %d trace lines: %+v", len(got), got)
	}
	if got[0].Status != "not proposed" || !strings.Contains(got[0].Detail, "cosine") {
		t.Errorf("first line = %+v", got[0])
	}
	if got[1].Status != "proposed" || got[1].Detail != "neither" {
		t.Errorf("second line = %+v, want verdict neither", got[1])
	}
}
