package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func setupMockCommandAPI(t *testing.T) *httptest.Server {
	t.Helper()
	fixture, err := os.ReadFile("e2e/fixtures/commands_hp.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/command/hp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	})
	mux.HandleFunc("/api/command", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Command string `json:"command"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Command {
		case "gold":
			_ = json.NewEncoder(w).Encode(CommandResult{Content: "黄金：1001 元/克", Type: "text"})
		case "a 你好":
			_ = json.NewEncoder(w).Encode(CommandResult{Content: "鸡哥说：你好", Type: "text"})
		case "img":
			_ = json.NewEncoder(w).Encode(CommandResult{Content: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB", Type: "image"})
		default:
			http.Error(w, `{"error":"unexpected command"}`, http.StatusBadRequest)
		}
	})
	return httptest.NewServer(mux)
}

func setupMockDB(t *testing.T) (sqlmock.Sqlmock, func()) {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	db = mockDB
	cleanup := func() {
		_ = mockDB.Close()
		db = nil
	}
	return mock, cleanup
}

func signBody(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + ":"))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestMockManifestDirectCommands(t *testing.T) {
	ts := setupMockCommandAPI(t)
	defer ts.Close()

	cfg = Config{CommandAPIBaseURL: ts.URL + "/api", CommandAPITimeoutMS: 2000}
	httpClient = &http.Client{Timeout: 2 * time.Second}

	req := httptest.NewRequest(http.MethodGet, "/manifest.json", nil)
	w := httptest.NewRecorder()
	handleManifest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("manifest decode: %v", err)
	}

	tools := body["tools"].([]any)
	seenGold := false
	seenA := false
	for _, item := range tools {
		m := item.(map[string]any)
		if m["name"] == "gold" && m["command"] == "gold" {
			seenGold = true
		}
		if m["name"] == "a" && m["command"] == "a" && m["parameters"] != nil {
			seenA = true
		}
	}
	if !seenGold || !seenA {
		t.Fatalf("expected tool registration, got %s", w.Body.String())
	}
}

func TestMockHubWebhookGoldCommand(t *testing.T) {
	ts := setupMockCommandAPI(t)
	defer ts.Close()
	mock, cleanup := setupMockDB(t)
	defer cleanup()

	cfg = Config{CommandAPIBaseURL: ts.URL + "/api", CommandAPITimeoutMS: 2000}
	httpClient = &http.Client{Timeout: 2 * time.Second}

	rows := sqlmock.NewRows([]string{"id", "app_token", "signing_secret", "bot_id", "handle"}).
		AddRow("inst_1", "app_xxx", "secret123", "bot_1", "")
	mock.ExpectQuery("SELECT id, app_token, signing_secret, bot_id, handle FROM installations WHERE id=\\$1").
		WithArgs("inst_1").
		WillReturnRows(rows)

	envelope := HubEvent{V: 1, Type: "event", TraceID: "tr_test", InstallationID: "inst_1"}
	envelope.Event.Type = "command"
	envelope.Event.Data = map[string]any{"command": "gold", "text": "", "args": nil}
	body, _ := json.Marshal(envelope)
	tsValue := "1710000000"

	req := httptest.NewRequest(http.MethodPost, "/hub/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Timestamp", tsValue)
	req.Header.Set("X-Signature", signBody("secret123", tsValue, body))
	w := httptest.NewRecorder()

	handleHubWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["reply"] != "黄金：1001 元/克" {
		t.Fatalf("reply=%q", resp["reply"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestMockHubWebhookCommandWithArgs(t *testing.T) {
	ts := setupMockCommandAPI(t)
	defer ts.Close()
	mock, cleanup := setupMockDB(t)
	defer cleanup()

	cfg = Config{CommandAPIBaseURL: ts.URL + "/api", CommandAPITimeoutMS: 2000}
	httpClient = &http.Client{Timeout: 2 * time.Second}

	rows := sqlmock.NewRows([]string{"id", "app_token", "signing_secret", "bot_id", "handle"}).
		AddRow("inst_1", "app_xxx", "secret123", "bot_1", "")
	mock.ExpectQuery("SELECT id, app_token, signing_secret, bot_id, handle FROM installations WHERE id=\\$1").
		WithArgs("inst_1").
		WillReturnRows(rows)

	envelope := HubEvent{V: 1, Type: "event", TraceID: "tr_test_2", InstallationID: "inst_1"}
	envelope.Event.Type = "command"
	envelope.Event.Data = map[string]any{"command": "a", "text": "你好", "args": nil}
	body, _ := json.Marshal(envelope)
	tsValue := "1710000001"

	req := httptest.NewRequest(http.MethodPost, "/hub/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Timestamp", tsValue)
	req.Header.Set("X-Signature", signBody("secret123", tsValue, body))
	w := httptest.NewRecorder()

	handleHubWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["reply"] != "鸡哥说：你好" {
		t.Fatalf("reply=%q", resp["reply"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestMockHubWebhookCommandWithStructuredArgs(t *testing.T) {
	ts := setupMockCommandAPI(t)
	defer ts.Close()
	mock, cleanup := setupMockDB(t)
	defer cleanup()

	cfg = Config{CommandAPIBaseURL: ts.URL + "/api", CommandAPITimeoutMS: 2000}
	httpClient = &http.Client{Timeout: 2 * time.Second}

	rows := sqlmock.NewRows([]string{"id", "app_token", "signing_secret", "bot_id", "handle"}).
		AddRow("inst_1", "app_xxx", "secret123", "bot_1", "")
	mock.ExpectQuery("SELECT id, app_token, signing_secret, bot_id, handle FROM installations WHERE id=\\$1").
		WithArgs("inst_1").
		WillReturnRows(rows)

	// AI Agent triggers with structured args
	envelope := HubEvent{V: 1, Type: "event", TraceID: "tr_test_3", InstallationID: "inst_1"}
	envelope.Event.Type = "command"
	envelope.Event.Data = map[string]any{
		"command": "a",
		"text":    "",
		"args":    map[string]any{"text": "你好"},
	}
	body, _ := json.Marshal(envelope)
	tsValue := "1710000002"

	req := httptest.NewRequest(http.MethodPost, "/hub/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Timestamp", tsValue)
	req.Header.Set("X-Signature", signBody("secret123", tsValue, body))
	w := httptest.NewRecorder()

	handleHubWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["reply"] != "鸡哥说：你好" {
		t.Fatalf("reply=%q", resp["reply"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestMockImageResultFallsBackToTextNotice(t *testing.T) {
	ts := setupMockCommandAPI(t)
	defer ts.Close()

	cfg = Config{CommandAPIBaseURL: ts.URL + "/api", CommandAPITimeoutMS: 2000}
	httpClient = &http.Client{Timeout: 2 * time.Second}

	result, err := executeCommandServiceCommand(rctx(), "img")
	if err != nil {
		t.Fatalf("execute img: %v", err)
	}
	text := renderReply(result)
	if !strings.Contains(text, "图片") {
		t.Fatalf("expected image fallback notice, got %q", text)
	}
}
