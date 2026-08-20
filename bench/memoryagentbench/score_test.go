package main

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestAnswerHit(t *testing.T) {
	cases := []struct {
		content string
		answers []string
		want    bool
	}{
		{"Chanel was founded by Andy Warhol.", []string{"Andy Warhol"}, true},
		{"Chanel was founded by Andy Warhol.", []string{"andy warhol"}, true},
		{"Chanel was founded by Coco Chanel.", []string{"Andy Warhol"}, false},
		{"Chanel was founded by Coco Chanel.", []string{"Coco Chanel", "Andy Warhol"}, true},
	}
	for _, c := range cases {
		if got := answerHit(c.content, c.answers); got != c.want {
			t.Errorf("answerHit(%q, %v) = %v, want %v", c.content, c.answers, got, c.want)
		}
	}
}

func TestTopKHit(t *testing.T) {
	results := []memory.Memory{
		{Content: "Chanel was founded by Coco Chanel."},
		{Content: "Chanel was founded by Andy Warhol."},
	}
	if topKHit(results, []string{"Andy Warhol"}, 1) {
		t.Error("top-1 should miss: Andy Warhol is ranked second")
	}
	if !topKHit(results, []string{"Andy Warhol"}, 2) {
		t.Error("top-2 should hit")
	}
	if topKHit(results, []string{"nonexistent"}, 2) {
		t.Error("expected no hit for an absent answer")
	}
	if topKHit(nil, []string{"anything"}, 5) {
		t.Error("expected no hit against an empty result set")
	}
}
