// Package procgroup holds the process-group mechanics shared by the two
// places Ghost confines a child that may spawn helpers of its own: the
// lifecycle phase spawn (cmd/ghost) and the LLM-harness spawn (internal/ai).
// Both need the same four moves — put the child in a group of its own, ask the
// whole group to stop, force-kill the whole group, and ask whether anything is
// still in it — and one implementation is what keeps the two from drifting
// apart. The platform differences live here: on Windows there is no portable
// group signal, so the direct child is what gets killed.
//
// One asymmetry is deliberate and must not be "reconciled": SetGroup on Windows
// sets CREATE_NEW_PROCESS_GROUP, and only the HARNESS spawn calls it. The
// lifecycle phase spawn does not, because a phase is ghost itself and has to
// keep receiving the console Ctrl-C that maintenanceContext relies on; see
// cmd/ghost/lifecycle_windows.go.
package procgroup
