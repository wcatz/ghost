package main

import (
	"strings"
	"testing"
)

// TestReflectProjectHeaderRedactsTokenShapedProjectID asserts the reflect
// phase header withholds a project id shaped like a GitHub personal access
// token rather than printing it verbatim (#1895), and still renders an
// ordinary id as-is.
func TestReflectProjectHeaderRedactsTokenShapedProjectID(t *testing.T) {
	cred := reflectCredential()
	out := reflectProjectHeader("reflect-project", cred)
	if strings.Contains(out, cred) {
		t.Fatalf("project header printed verbatim credential: %q", out)
	}
	if !strings.Contains(out, "<project withheld: it holds a GitHub personal access token>") {
		t.Fatalf("project header missing withheld rendering: %q", out)
	}
	if !strings.HasPrefix(out, "Project:      reflect-project (") {
		t.Fatalf("project header lost its label: %q", out)
	}

	plain := reflectProjectHeader("ghost", "6bdc098af7f5")
	if want := "Project:      ghost (6bdc098af7f5)"; plain != want {
		t.Fatalf("plain header = %q, want %q", plain, want)
	}
}
