package main

import (
	"reflect"
	"testing"
)

func TestSessionsInRankOrder(t *testing.T) {
	memToSession := map[string]string{
		"m1": "sA", "m2": "sB", "m3": "sA", "m4": "sC",
	}
	got := sessionsInRankOrder([]string{"m3", "m1", "m4", "m2", "unknown"}, memToSession)
	want := []string{"sA", "sC", "sB"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sessionsInRankOrder = %v, want %v", got, want)
	}
}

func TestIsAbstention(t *testing.T) {
	if !isAbstention("gpt4_deadbeef_abs") {
		t.Error("suffix _abs must be abstention")
	}
	if isAbstention("gpt4_deadbeef") {
		t.Error("plain id must not be abstention")
	}
}
