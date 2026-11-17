package main

import "strings"

// qualify says why a finished run cannot be scored, or "" when it counts. The
// Ghost arm has to show that Ghost was used, that a session-start block reached
// a session, and that the run got as far as every stage and probe it declares.
// A baseline arm runs no arc stages and no probes, so reaching the last stage is
// its bar; it is not disqualified for lacking the very thing it measures against.
// The first thing missing is the reason reported for the run.
func qualify(res *Result) string {
	ghostArm := res.Arm() == armWithGhost
	if ghostArm && !sawGhostCall(res) {
		return "no ghost tool call was observed"
	}
	if ghostArm && !sawInjection(res) {
		return "no session-start block reached a session"
	}
	if len(res.Sessions) < len(res.Story.Stages) {
		return "the run ended before the probe session"
	}
	if ghostArm && !probesReached(res) {
		return "the run ended before the probe session"
	}
	return ""
}

// sawGhostCall reads the effects only a Ghost invocation leaves on a Result: a
// save's raw output, a rendered block, a probe's rows, an arc phase's output or
// the end state read from the scratch store. A run with none of them never
// reached the mechanism, whatever its answer says.
func sawGhostCall(res *Result) bool {
	if len(res.Opening) > 0 || len(res.SaveOutputs) > 0 || len(res.Probes) > 0 ||
		res.FinalBlock != "" || res.Supersede != "" || res.Resolve != "" ||
		len(res.State.Links) > 0 || len(res.State.Stamps) > 0 {
		return true
	}
	return sawInjection(res)
}

func sawInjection(res *Result) bool {
	for _, sess := range res.Sessions {
		if strings.TrimSpace(sess.Block) != "" {
			return true
		}
	}
	return false
}

// probesReached asks for a result for every probe the storyline declares, at its
// stage or at the end of the arc. A probe that ran but returned no rows still has
// an entry; a missing entry means the run never got there.
func probesReached(res *Result) bool {
	for i, stage := range res.Story.Stages {
		for _, p := range stage.Probes {
			if i >= len(res.Sessions) || res.Sessions[i].Probes == nil {
				return false
			}
			if _, ok := res.Sessions[i].Probes[p.Name]; !ok {
				return false
			}
		}
	}
	for _, p := range res.Story.Probes {
		if res.Probes == nil {
			return false
		}
		if _, ok := res.Probes[p.Name]; !ok {
			return false
		}
	}
	return true
}
