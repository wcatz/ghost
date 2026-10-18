package mcpserver

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// taskNotifyProject is the project every test below writes to. newStoreWithDB
// registers it under this id with the name "test-project".
const taskNotifyProject = "abc123"

// The tasks resource is documented as ghost://project/{project_id}/tasks, and a
// task write must push resources/updated for it, or a client that pinned the
// resource keeps a stale task list. One test per tool, because the three
// notifications sit at three separate call sites.

func TestTaskCreateNotifiesTasksResourceSubscriber(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	const tasksURI = "ghost://project/" + taskNotifyProject + "/tasks"
	session, updated := subscribeTestClient(t, srv, tasksURI)

	res := callTool(t, session, "ghost_task_create", map[string]any{
		"project_id": taskNotifyProject, "title": "a task",
	})
	if res.IsError {
		t.Fatalf("ghost_task_create failed: %s", resultText(res))
	}
	awaitNotification(t, updated, tasksURI)
}

// seedTask creates one task before the caller subscribes, so the create's own
// notification cannot be mistaken for the write under test.
func seedTask(t *testing.T, srv *Server) string {
	t.Helper()
	session := connectedClient(t, srv)
	res := callTool(t, session, "ghost_task_create", map[string]any{
		"project_id": taskNotifyProject, "title": "seed task",
	})
	if res.IsError {
		t.Fatalf("seed ghost_task_create failed: %s", resultText(res))
	}
	id, ok := extractID(resultText(res))
	if !ok {
		t.Fatalf("no task id in %q", resultText(res))
	}
	return id
}

func TestTaskCompleteNotifiesTasksResourceSubscriber(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	id := seedTask(t, srv)
	const tasksURI = "ghost://project/" + taskNotifyProject + "/tasks"
	session, updated := subscribeTestClient(t, srv, tasksURI)

	res := callTool(t, session, "ghost_task_complete", map[string]any{"task_id": id})
	if res.IsError {
		t.Fatalf("ghost_task_complete failed: %s", resultText(res))
	}
	awaitNotification(t, updated, tasksURI)
}

func TestTaskUpdateNotifiesTasksResourceSubscriber(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	id := seedTask(t, srv)
	const tasksURI = "ghost://project/" + taskNotifyProject + "/tasks"
	session, updated := subscribeTestClient(t, srv, tasksURI)

	res := callTool(t, session, "ghost_task_update", map[string]any{"task_id": id, "status": "active"})
	if res.IsError {
		t.Fatalf("ghost_task_update failed: %s", resultText(res))
	}
	awaitNotification(t, updated, tasksURI)
}

// decisionStatusAtSend records, for every resources/updated notification the
// server is about to send, the status the decision row holds at that instant.
// It is a sending middleware, so it runs on the writer's own goroutine inside
// the notify call: the read happens before the notification leaves the server
// and the tool handler cannot have moved on, which makes the observation
// independent of scheduling. A client-side handler could not give that, because
// it runs after the server has already carried on to its next step.
type decisionStatusAtSend struct {
	mu       sync.Mutex
	statuses map[string][]string // notified URI -> status read at each send
	db       *sql.DB
	decision string
}

func (o *decisionStatusAtSend) middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "notifications/resources/updated" {
			if p, ok := req.GetParams().(*mcp.ResourceUpdatedNotificationParams); ok {
				var status string
				if err := o.db.QueryRow(`SELECT status FROM decisions WHERE id = ?`, o.decision).Scan(&status); err != nil {
					status = "read failed: " + err.Error()
				}
				o.mu.Lock()
				o.statuses[p.URI] = append(o.statuses[p.URI], status)
				o.mu.Unlock()
			}
		}
		return next(ctx, method, req)
	}
}

// A decision write notifies only after its supersedes step: a subscriber woken
// by the push re-reads the resource, and a push sent before the old decision is
// marked superseded wakes it to read a state that has not changed yet. The rule
// is stated on the observable: when the notification for the decisions resource
// and the one for the context resource leave the server, the old decision
// already reads as superseded. It holds whether the supersede is its own write
// or part of the one that records the new decision.
func TestDecisionRecordNotifiesOnlyAfterSupersedeIsVisible(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	const oldID = "olddecision1"
	plantDecision(t, db, taskNotifyProject, oldID, "old", "old decision", "old rationale", "active")

	obs := &decisionStatusAtSend{statuses: map[string][]string{}, db: db, decision: oldID}
	srv.mcp.AddSendingMiddleware(obs.middleware)

	decisionsURI := "ghost://project/" + taskNotifyProject + "/decisions"
	contextURI := "ghost://project/" + taskNotifyProject + "/context"
	session, _ := subscribeTestClient(t, srv, decisionsURI, contextURI)

	// The subscription checks sent through the middleware too; only sends from
	// the write under test count.
	obs.mu.Lock()
	obs.statuses = map[string][]string{}
	obs.mu.Unlock()

	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": taskNotifyProject, "title": "new", "decision": "new decision",
		"rationale": "new rationale", "supersedes": oldID,
	})
	if res.IsError {
		t.Fatalf("ghost_decision_record failed: %s", resultText(res))
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	for _, uri := range []string{decisionsURI, contextURI} {
		got := obs.statuses[uri]
		if len(got) == 0 {
			t.Errorf("no notification for %q left the server", uri)
		}
		for _, status := range got {
			if status != "superseded" {
				t.Errorf("notification for %q was sent while the superseded decision read %q, want \"superseded\"", uri, status)
			}
		}
	}
}
