package telegram

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// testBot creates a minimal Bot for testing HTTP client functions.
func testBot(t *testing.T, serverURL string) *Bot {
	t.Helper()
	addr := strings.TrimPrefix(serverURL, "http://")
	return &Bot{
		serverAddr:   addr,
		serverToken:  "test-token",
		pendingChat:  make(map[int64]string),
		sessionCosts: make(map[string]string),
	}
}

// --- fetchSessions ---

func TestFetchSessions_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header = %q, want %q", got, "Bearer test-token")
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]apiSession{
			{ID: "session-abc-123", ProjectPath: "/home/user/proj", ProjectName: "proj", Mode: "chat", Active: true, Messages: 5},
			{ID: "session-def-456", ProjectPath: "/home/user/other", ProjectName: "other", Mode: "code", Active: true, Messages: 12},
		})
	}))
	defer server.Close()

	// Override package-level httpClient for test.
	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	sessions, err := tb.fetchSessions()
	if err != nil {
		t.Fatalf("fetchSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	if sessions[0].ProjectName != "proj" {
		t.Errorf("first session name = %q, want %q", sessions[0].ProjectName, "proj")
	}
	if sessions[1].Messages != 12 {
		t.Errorf("second session messages = %d, want 12", sessions[1].Messages)
	}
}

func TestFetchSessions_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	_, err := tb.fetchSessions()
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should contain status code, got: %v", err)
	}
}

func TestFetchSessions_NoAuthToken(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode([]apiSession{})
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	tb.serverToken = "" // no token
	_, err := tb.fetchSessions()
	if err != nil {
		t.Fatalf("fetchSessions: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("should not send auth header when token is empty, got %q", gotAuth)
	}
}

// --- sendChatMessage ---

func TestSendChatMessage_SSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/send") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		// Verify request body.
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["message"] != "hello ghost" {
			t.Errorf("message = %q, want %q", body["message"], "hello ghost")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: text\ndata: {\"text\":\"Hello \"}\n\n")
		fmt.Fprint(w, "event: text\ndata: {\"text\":\"world!\"}\n\n")
		fmt.Fprint(w, "event: done\ndata: {\"session_cost\":\"$0.12\"}\n\n")
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	response, err := tb.sendChatMessage("session-abc", "hello ghost")
	if err != nil {
		t.Fatalf("sendChatMessage: %v", err)
	}
	if response != "Hello world!" {
		t.Errorf("response = %q, want %q", response, "Hello world!")
	}

	// Verify cost was tracked.
	tb.mu.Lock()
	cost := tb.sessionCosts["session-abc"]
	tb.mu.Unlock()
	if cost != "$0.12" {
		t.Errorf("session cost = %q, want %q", cost, "$0.12")
	}
}

func TestSendChatMessage_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	_, err := tb.sendChatMessage("session-abc", "test")
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should contain status code, got: %v", err)
	}
}

func TestSendChatMessage_EmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: done\ndata: {}\n\n")
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	response, err := tb.sendChatMessage("session-abc", "test")
	if err != nil {
		t.Fatalf("sendChatMessage: %v", err)
	}
	if response != "" {
		t.Errorf("expected empty response, got %q", response)
	}
}

// --- createMemory ---

func TestCreateMemory_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/memories/" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}

		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if body["project_id"] != "proj-123" {
			t.Errorf("project_id = %v, want %q", body["project_id"], "proj-123")
		}
		if body["content"] != "Go uses goroutines for concurrency" {
			t.Errorf("content = %v", body["content"])
		}
		if body["source"] != "telegram" {
			t.Errorf("source = %v, want %q", body["source"], "telegram")
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":     "mem-abc",
			"merged": false,
		})
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	id, merged, err := tb.createMemory("proj-123", "Go uses goroutines for concurrency")
	if err != nil {
		t.Fatalf("createMemory: %v", err)
	}
	if id != "mem-abc" {
		t.Errorf("id = %q, want %q", id, "mem-abc")
	}
	if merged {
		t.Error("expected merged=false")
	}
}

func TestCreateMemory_Merged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":     "mem-existing",
			"merged": true,
		})
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	_, merged, err := tb.createMemory("proj-123", "some content")
	if err != nil {
		t.Fatalf("createMemory: %v", err)
	}
	if !merged {
		t.Error("expected merged=true")
	}
}

func TestCreateMemory_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("db error"))
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	_, _, err := tb.createMemory("proj-123", "content")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

// --- deleteMemory ---

func TestDeleteMemory_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/mem-abc") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	err := tb.deleteMemory("mem-abc")
	if err != nil {
		t.Fatalf("deleteMemory: %v", err)
	}
}

func TestDeleteMemory_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	err := tb.deleteMemory("nonexistent")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

// --- resolveSessionID ---

func TestResolveSessionID_Found(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]apiSession{
			{ID: "abcdef01-1234-5678-9abc-def012345678", ProjectName: "ghost"},
			{ID: "98765432-abcd-efgh-ijkl-mnopqrstuvwx", ProjectName: "other"},
		})
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	fullID, err := tb.resolveSessionID("abcdef01")
	if err != nil {
		t.Fatalf("resolveSessionID: %v", err)
	}
	if fullID != "abcdef01-1234-5678-9abc-def012345678" {
		t.Errorf("fullID = %q, want full UUID", fullID)
	}
}

func TestResolveSessionID_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]apiSession{
			{ID: "abcdef01-1234", ProjectName: "ghost"},
		})
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	_, err := tb.resolveSessionID("zzzzz")
	if err == nil {
		t.Fatal("expected error for no match")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want 'not found'", err)
	}
}

// --- setSessionMode ---

func TestSetSessionMode_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/mode") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["mode"] != "code" {
			t.Errorf("mode = %q, want %q", body["mode"], "code")
		}

		json.NewEncoder(w).Encode(map[string]string{"mode": "code"})
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	mode, err := tb.setSessionMode("session-abc", "code")
	if err != nil {
		t.Fatalf("setSessionMode: %v", err)
	}
	if mode != "code" {
		t.Errorf("mode = %q, want %q", mode, "code")
	}
}

// --- callApproveAPI ---

func TestCallApproveAPI_Approved(t *testing.T) {
	var receivedPayload map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/approve") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&receivedPayload)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	err := tb.callApproveAPI("session-abc", true, "")
	if err != nil {
		t.Fatalf("callApproveAPI: %v", err)
	}
	if receivedPayload["approved"] != true {
		t.Errorf("approved = %v, want true", receivedPayload["approved"])
	}
	if _, ok := receivedPayload["instructions"]; ok {
		t.Error("instructions should not be sent when empty")
	}
}

func TestCallApproveAPI_DeniedWithInstructions(t *testing.T) {
	var receivedPayload map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedPayload)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	err := tb.callApproveAPI("session-abc", false, "use a safer command")
	if err != nil {
		t.Fatalf("callApproveAPI: %v", err)
	}
	if receivedPayload["approved"] != false {
		t.Errorf("approved = %v, want false", receivedPayload["approved"])
	}
	if receivedPayload["instructions"] != "use a safer command" {
		t.Errorf("instructions = %v, want %q", receivedPayload["instructions"], "use a safer command")
	}
}

func TestCallApproveAPI_NoServerAddr(t *testing.T) {
	tb := &Bot{}
	err := tb.callApproveAPI("session-abc", true, "")
	if err == nil {
		t.Fatal("expected error when server address not configured")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error = %v, want 'not configured'", err)
	}
}

func TestCallApproveAPI_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	origClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = origClient })

	tb := testBot(t, server.URL)
	err := tb.callApproveAPI("session-abc", true, "")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

// --- ApprovalResolved ---

func TestApprovalResolved_MatchingSession(t *testing.T) {
	tb := &Bot{
		pendingChat:  make(map[int64]string),
		sessionCosts: make(map[string]string),
	}
	tb.approval.sessionID = "session-abc"
	tb.approval.streamID = "stream-1"
	tb.approval.toolName = "bash"
	// Leave messageIDs nil — deleteApprovalMessages is a no-op when nil,
	// avoiding the need for a real telegram bot instance.

	tb.ApprovalResolved("session-abc", true)

	tb.approval.mu.Lock()
	defer tb.approval.mu.Unlock()
	if tb.approval.sessionID != "" {
		t.Error("sessionID should be cleared after resolve")
	}
	if tb.approval.toolName != "" {
		t.Error("toolName should be cleared after resolve")
	}
}

func TestApprovalResolved_NonMatchingSession(t *testing.T) {
	tb := &Bot{
		pendingChat:  make(map[int64]string),
		sessionCosts: make(map[string]string),
	}
	tb.approval.sessionID = "session-abc"
	tb.approval.toolName = "bash"

	// Resolve a different session — should be a no-op.
	tb.ApprovalResolved("session-xyz", true)

	tb.approval.mu.Lock()
	defer tb.approval.mu.Unlock()
	if tb.approval.sessionID != "session-abc" {
		t.Error("sessionID should not be cleared for non-matching session")
	}
}

// --- SetServerAddr / SetServerToken ---

func TestSetServerAddr(t *testing.T) {
	tb := &Bot{}
	tb.SetServerAddr("localhost:2187")
	if tb.serverAddr != "localhost:2187" {
		t.Errorf("serverAddr = %q, want %q", tb.serverAddr, "localhost:2187")
	}
}

func TestSetServerToken(t *testing.T) {
	tb := &Bot{}
	tb.SetServerToken("my-token")
	if tb.serverToken != "my-token" {
		t.Errorf("serverToken = %q, want %q", tb.serverToken, "my-token")
	}
}

// --- pendingChat state ---

func TestPendingChat_ConcurrentAccess(t *testing.T) {
	tb := &Bot{
		pendingChat:  make(map[int64]string),
		sessionCosts: make(map[string]string),
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(chatID int64) {
			defer wg.Done()
			tb.mu.Lock()
			tb.pendingChat[chatID] = "session-" + fmt.Sprintf("%d", chatID)
			tb.mu.Unlock()
		}(int64(i))
	}
	wg.Wait()

	if len(tb.pendingChat) != 50 {
		t.Errorf("expected 50 pending chats, got %d", len(tb.pendingChat))
	}
}
