package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupMockCommandAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
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
			_ = json.NewEncoder(w).Encode(CommandResult{Content: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB", Type: "image", Name: "random.png"})
		default:
			http.Error(w, `{"error":"unexpected command"}`, http.StatusBadRequest)
		}
	})
	return httptest.NewServer(mux)
}

func setupTestDB(t *testing.T) func() {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	var err error
	db, err = sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return func() {
		db.Close()
		os.Remove(dbPath)
		db = nil
	}
}

func seedInstallation(t *testing.T, id, appToken, webhookSecret, botID string) {
	t.Helper()
	_, err := db.Exec("INSERT INTO installations (id, app_token, webhook_secret, bot_id) VALUES (?, ?, ?, ?)",
		id, appToken, webhookSecret, botID)
	if err != nil {
		t.Fatalf("seed installation: %v", err)
	}
}

func signBody(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + ":"))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestMockHubWebhookGoldCommand(t *testing.T) {
	ts := setupMockCommandAPI(t)
	defer ts.Close()
	cleanup := setupTestDB(t)
	defer cleanup()
	seedInstallation(t, "inst_1", "app_xxx", "secret123", "bot_1")

	cfg = Config{CommandAPIBaseURL: ts.URL + "/api", CommandAPITimeoutMS: 2000, SyncDeadlineMS: 2000}
	commandClient = &http.Client{Timeout: 2 * time.Second}
	botClient = &http.Client{Timeout: 2 * time.Second}

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
}

func TestMockHubWebhookCommandWithArgs(t *testing.T) {
	ts := setupMockCommandAPI(t)
	defer ts.Close()
	cleanup := setupTestDB(t)
	defer cleanup()
	seedInstallation(t, "inst_1", "app_xxx", "secret123", "bot_1")

	cfg = Config{CommandAPIBaseURL: ts.URL + "/api", CommandAPITimeoutMS: 2000, SyncDeadlineMS: 2000}
	commandClient = &http.Client{Timeout: 2 * time.Second}
	botClient = &http.Client{Timeout: 2 * time.Second}

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
}

func TestMockHubWebhookCommandWithStructuredArgs(t *testing.T) {
	ts := setupMockCommandAPI(t)
	defer ts.Close()
	cleanup := setupTestDB(t)
	defer cleanup()
	seedInstallation(t, "inst_1", "app_xxx", "secret123", "bot_1")

	cfg = Config{CommandAPIBaseURL: ts.URL + "/api", CommandAPITimeoutMS: 2000, SyncDeadlineMS: 2000}
	commandClient = &http.Client{Timeout: 2 * time.Second}
	botClient = &http.Client{Timeout: 2 * time.Second}

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
}

func TestMockImageReplyWithName(t *testing.T) {
	ts := setupMockCommandAPI(t)
	defer ts.Close()

	cfg = Config{CommandAPIBaseURL: ts.URL + "/api", CommandAPITimeoutMS: 2000, SyncDeadlineMS: 2000}
	commandClient = &http.Client{Timeout: 2 * time.Second}

	result, err := executeCommandServiceCommand(rctx(), "img")
	if err != nil {
		t.Fatalf("execute img: %v", err)
	}
	reply := resolveReply(result)
	if reply.MsgType != "image" || reply.MediaBase64 == "" {
		t.Fatalf("expected base64 image reply, got type=%q base64=%q text=%q", reply.MsgType, reply.MediaBase64, reply.Text)
	}
	if reply.MediaName != "random.png" {
		t.Fatalf("expected MediaName=random.png, got %q", reply.MediaName)
	}

	w := httptest.NewRecorder()
	writeSyncReply(w, reply)
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode sync reply: %v", err)
	}
	if resp["reply_name"] != "random.png" {
		t.Fatalf("expected reply_name=random.png, got %q", resp["reply_name"])
	}
}

func TestOAuthSetupRedirect(t *testing.T) {
	cfg = Config{
		HubURL:  "https://hub.example.com",
		AppID:   "app_123",
		BaseURL: "https://myapp.example.com",
	}

	req := httptest.NewRequest(http.MethodGet, "/oauth/setup?bot_id=bot_456", nil)
	w := httptest.NewRecorder()

	handleOAuthSetup(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://hub.example.com/api/apps/app_123/oauth/authorize?") {
		t.Fatalf("unexpected redirect: %s", loc)
	}
	if !strings.Contains(loc, "bot_id=bot_456") {
		t.Fatalf("missing bot_id in redirect: %s", loc)
	}
	if !strings.Contains(loc, "code_challenge=") {
		t.Fatalf("missing code_challenge in redirect: %s", loc)
	}
	if !strings.Contains(loc, "state=") {
		t.Fatalf("missing state in redirect: %s", loc)
	}
}

func TestOAuthSetupMissingAppID(t *testing.T) {
	cfg = Config{AppID: ""}

	req := httptest.NewRequest(http.MethodGet, "/oauth/setup", nil)
	w := httptest.NewRecorder()

	handleOAuthSetup(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestURLVerification(t *testing.T) {
	body := `{"v":1,"type":"url_verification","challenge":"test_challenge_123"}`
	req := httptest.NewRequest(http.MethodPost, "/hub/webhook", strings.NewReader(body))
	w := httptest.NewRecorder()

	handleHubWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["challenge"] != "test_challenge_123" {
		t.Fatalf("challenge=%q", resp["challenge"])
	}
}

func TestMigrationIdempotent(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	// Running migrate again should not fail.
	if err := migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	// Verify table works.
	seedInstallation(t, "test_1", "tok", "sec", "bot")
	inst := getInstallation("test_1")
	if inst == nil || inst.AppToken != "tok" {
		t.Fatalf("expected installation, got %+v", inst)
	}
}
