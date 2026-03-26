package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Config struct {
	Port                string
	HubURL              string
	DBPath              string
	BaseURL             string
	AppID               string
	CommandAPIBaseURL   string
	CommandAPITimeoutMS int
	SyncDeadlineMS      int
}

type Installation struct {
	ID            string
	AppToken      string
	WebhookSecret string
	BotID         string
}

type HubEvent struct {
	V              int    `json:"v"`
	Type           string `json:"type"`
	TraceID        string `json:"trace_id"`
	InstallationID string `json:"installation_id"`
	Bot            struct {
		ID string `json:"id"`
	} `json:"bot"`
	Event struct {
		Type      string         `json:"type"`
		ID        string         `json:"id"`
		Timestamp int64          `json:"timestamp"`
		Data      map[string]any `json:"data"`
	} `json:"event"`
}

type CommandResult struct {
	Content string `json:"content"`
	Type    string `json:"type"`
	Name    string `json:"name,omitempty"`
}

type Reply struct {
	Text        string
	MsgType     string
	MediaURL    string
	MediaBase64 string
	MediaName   string
}

// PKCE state for in-flight OAuth flows.
type pkceEntry struct {
	CodeVerifier string
	HubURL       string // Hub URL from the setup request
	AppID        string // App ID from the setup request
	ExpiresAt    time.Time
}

var (
	cfg           Config
	db            *sql.DB
	commandClient *http.Client // upstream command API (long timeout)
	botClient     *http.Client // Hub Bot API (short timeout)

	pkceStates   = map[string]pkceEntry{}
	pkceStatesMu sync.Mutex
)

func main() {
	cfg = Config{
		Port:                envOr("PORT", "8081"),
		HubURL:              strings.TrimRight(envOr("HUB_URL", "https://hub.openilink.com"), "/"),
		DBPath:              envOr("DB_PATH", "/data/command-service.db"),
		BaseURL:             strings.TrimRight(os.Getenv("BASE_URL"), "/"),
		AppID:               os.Getenv("APP_ID"),
		CommandAPIBaseURL:   strings.TrimRight(envOr("COMMAND_API_BASE_URL", "https://bhwa233-api.vercel.app/api"), "/"),
		CommandAPITimeoutMS: envIntOr("COMMAND_API_TIMEOUT_MS", 120000),
		SyncDeadlineMS:      envIntOr("SYNC_DEADLINE_MS", 2000),
	}

	commandClient = &http.Client{Timeout: time.Duration(cfg.CommandAPITimeoutMS) * time.Millisecond}
	botClient = &http.Client{Timeout: 15 * time.Second}

	// Ensure DB directory exists.
	if dir := filepath.Dir(cfg.DBPath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}

	var err error
	db, err = sql.Open("sqlite3", cfg.DBPath+"?_journal_mode=WAL")
	if err != nil {
		slog.Error("db open failed", "err", err)
		os.Exit(1)
	}
	if err := migrate(); err != nil {
		slog.Error("db migrate failed", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /hub/webhook", handleHubWebhook)
	mux.HandleFunc("GET /oauth/setup", handleOAuthSetup)
	mux.HandleFunc("GET /oauth/callback", handleOAuthCallback)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	addr := ":" + cfg.Port
	slog.Info("command service app starting", "addr", addr, "hub", cfg.HubURL, "db", cfg.DBPath, "command_api", cfg.CommandAPIBaseURL)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

// --- Migration (version-based) ---

func migrate() error {
	var version int
	_ = db.QueryRow("PRAGMA user_version").Scan(&version)

	if version < 1 {
		_, err := db.Exec(`CREATE TABLE IF NOT EXISTS installations (
			id TEXT PRIMARY KEY,
			app_token TEXT NOT NULL,
			webhook_secret TEXT NOT NULL,
			bot_id TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`)
		if err != nil {
			return err
		}
		if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
			return err
		}
	}

	return nil
}

// --- OAuth PKCE ---

func handleOAuthSetup(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Hub passes these params: ?hub={hub_url}&app_id={app_id}&bot_id={bot_id}&state={state}
	hubURL := strings.TrimRight(q.Get("hub"), "/")
	if hubURL == "" {
		hubURL = cfg.HubURL
	}
	appID := q.Get("app_id")
	if appID == "" {
		appID = cfg.AppID
	}
	if appID == "" {
		http.Error(w, "app_id not provided", http.StatusBadRequest)
		return
	}
	botID := q.Get("bot_id")
	hubState := q.Get("state") // state from Hub (passed through)

	verifier := generateRandomString(64)
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])

	localState := generateRandomString(32)

	pkceStatesMu.Lock()
	now := time.Now()
	for k, v := range pkceStates {
		if now.After(v.ExpiresAt) {
			delete(pkceStates, k)
		}
	}
	pkceStates[localState] = pkceEntry{
		CodeVerifier: verifier,
		HubURL:       hubURL,
		AppID:        appID,
		ExpiresAt:    now.Add(10 * time.Minute),
	}
	pkceStatesMu.Unlock()

	params := url.Values{}
	params.Set("bot_id", botID)
	params.Set("state", localState)
	params.Set("code_challenge", challenge)
	if hubState != "" {
		params.Set("hub_state", hubState)
	}

	redirectURL := fmt.Sprintf("%s/api/apps/%s/oauth/authorize?%s", hubURL, appID, params.Encode())
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}

	pkceStatesMu.Lock()
	entry, ok := pkceStates[state]
	if ok {
		delete(pkceStates, state)
	}
	pkceStatesMu.Unlock()

	if !ok || time.Now().After(entry.ExpiresAt) {
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
		return
	}

	payload, _ := json.Marshal(map[string]string{
		"code":          code,
		"code_verifier": entry.CodeVerifier,
	})

	hubURL := entry.HubURL
	if hubURL == "" {
		hubURL = cfg.HubURL
	}
	appID := entry.AppID
	if appID == "" {
		appID = cfg.AppID
	}
	exchangeURL := fmt.Sprintf("%s/api/apps/%s/oauth/exchange", hubURL, appID)
	req, err := http.NewRequest(http.MethodPost, exchangeURL, bytes.NewReader(payload))
	if err != nil {
		slog.Error("oauth exchange build request failed", "err", err)
		http.Error(w, "exchange request failed", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := botClient.Do(req)
	if err != nil {
		slog.Error("oauth exchange request failed", "err", err)
		http.Error(w, "exchange request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Error("oauth exchange failed", "status", resp.StatusCode, "body", string(body))
		http.Error(w, "exchange failed: "+string(body), resp.StatusCode)
		return
	}

	var result struct {
		InstallationID string `json:"installation_id"`
		AppToken       string `json:"app_token"`
		WebhookSecret  string `json:"webhook_secret"`
		BotID          string `json:"bot_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		slog.Error("oauth exchange decode failed", "err", err)
		http.Error(w, "decode failed", http.StatusInternalServerError)
		return
	}

	_, err = db.Exec(`INSERT INTO installations (id, app_token, webhook_secret, bot_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET app_token=excluded.app_token, webhook_secret=excluded.webhook_secret, bot_id=excluded.bot_id`,
		result.InstallationID, result.AppToken, result.WebhookSecret, result.BotID)
	if err != nil {
		slog.Error("save installation failed", "installation_id", result.InstallationID, "err", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}

	slog.Info("installation saved via oauth", "installation_id", result.InstallationID, "bot_id", result.BotID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"ok": "true", "installation_id": result.InstallationID})
}

func generateRandomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}

// --- Webhook ---

func handleHubWebhook(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	var event HubEvent
	if err := json.Unmarshal(body, &event); err != nil {
		slog.Warn("webhook: invalid json", "err", err)
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if event.Type == "url_verification" {
		var challenge struct {
			Challenge string `json:"challenge"`
		}
		_ = json.Unmarshal(body, &challenge)
		slog.Info("url verification", "challenge", challenge.Challenge)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"challenge": challenge.Challenge})
		return
	}

	inst := getInstallation(event.InstallationID)
	if inst == nil {
		slog.Warn("unknown installation", "installation_id", event.InstallationID, "trace_id", event.TraceID)
		http.Error(w, "unknown installation", http.StatusUnauthorized)
		return
	}

	timestamp := r.Header.Get("X-Timestamp")
	signature := r.Header.Get("X-Signature")
	expected := "sha256=" + computeSignature(inst.WebhookSecret, timestamp, body)
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		slog.Warn("invalid signature", "installation_id", inst.ID, "trace_id", event.TraceID)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	slog.Info("received event", "type", event.Type, "event_type", event.Event.Type, "installation", inst.ID, "trace_id", event.TraceID)

	if event.Event.Type == "command" {
		handleCommand(w, event, inst)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// --- Command handling ---

func handleCommand(w http.ResponseWriter, event HubEvent, inst *Installation) {
	data := event.Event.Data
	commandName, _ := data["command"].(string)
	text, _ := data["text"].(string)

	commandKey := strings.TrimSpace(strings.TrimPrefix(commandName, "/"))
	if commandKey == "" {
		slog.Warn("empty command received", "trace_id", event.TraceID)
		writeSyncReply(w, Reply{Text: "命令不能为空"})
		return
	}

	// Prefer structured args from AI Agent, fall back to free-form text.
	if argsRaw, ok := data["args"]; ok && argsRaw != nil {
		if argsMap, ok := argsRaw.(map[string]any); ok {
			if t, ok := argsMap["text"].(string); ok && t != "" {
				text = t
			}
		}
	}

	text = strings.TrimSpace(text)
	fullCommand := commandKey
	if text != "" {
		fullCommand += " " + text
	}

	slog.Info("executing command", "command", fullCommand, "trace_id", event.TraceID)

	// Try sync first; if upstream is slow, reply immediately and finish async.
	syncDeadline := time.Duration(cfg.SyncDeadlineMS) * time.Millisecond
	ctx, cancel := context.WithTimeout(rctx(), syncDeadline)
	defer cancel()

	result, err := executeCommandServiceCommand(ctx, fullCommand)
	if ctx.Err() == nil {
		if err != nil {
			slog.Error("sync command failed", "command", fullCommand, "err", err, "trace_id", event.TraceID)
			writeSyncReply(w, Reply{Text: friendlyError(err)})
			return
		}
		writeSyncReply(w, resolveReply(result))
		return
	}

	// Sync deadline exceeded — ack now, finish in background.
	slog.Info("command going async", "command", fullCommand, "trace_id", event.TraceID)
	writeSyncReply(w, Reply{Text: fmt.Sprintf("/%s 处理中，稍后推送结果…", commandKey)})

	replyTo := resolveReplyTo(data)
	if replyTo == "" {
		slog.Warn("async reply has no recipient, cannot push result", "command", fullCommand, "trace_id", event.TraceID)
		return
	}
	go func() {
		asyncCtx, asyncCancel := context.WithTimeout(rctx(), time.Duration(cfg.CommandAPITimeoutMS)*time.Millisecond)
		defer asyncCancel()

		asyncResult, asyncErr := executeCommandServiceCommand(asyncCtx, fullCommand)
		var reply Reply
		if asyncErr != nil {
			slog.Error("async command failed", "command", fullCommand, "err", asyncErr, "trace_id", event.TraceID)
			reply = Reply{Text: friendlyError(asyncErr)}
		} else {
			reply = resolveReply(asyncResult)
		}
		if err := sendBotMessage(inst.AppToken, replyTo, reply, event.TraceID); err != nil {
			slog.Error("bot send failed", "to", replyTo, "err", err, "trace_id", event.TraceID)
		}
	}()
}

func resolveReplyTo(data map[string]any) string {
	if g, ok := data["group"].(map[string]any); ok {
		if id, ok := g["id"].(string); ok && id != "" {
			return id
		}
	}
	if s, ok := data["sender"].(map[string]any); ok {
		if id, ok := s["id"].(string); ok && id != "" {
			return id
		}
	}
	return ""
}

// --- Bot API ---

func sendBotMessage(appToken, to string, reply Reply, traceID string) error {
	msg := map[string]string{
		"to":       to,
		"trace_id": traceID,
	}
	if reply.MediaURL != "" {
		msg["type"] = reply.MsgType
		msg["url"] = reply.MediaURL
		if reply.Text != "" {
			msg["content"] = reply.Text
		}
		if reply.MediaName != "" {
			msg["filename"] = reply.MediaName
		}
	} else if reply.MediaBase64 != "" {
		msg["type"] = reply.MsgType
		msg["base64"] = reply.MediaBase64
		if reply.MediaName != "" {
			msg["filename"] = reply.MediaName
		}
	} else {
		msg["type"] = "text"
		msg["content"] = reply.Text
	}

	payload, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(rctx(), http.MethodPost, cfg.HubURL+"/bot/v1/message/send", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+appToken)
	req.Header.Set("Content-Type", "application/json")
	if traceID != "" {
		req.Header.Set("X-Trace-Id", traceID)
	}

	resp, err := botClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("bot api %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	slog.Info("bot message sent", "to", to, "type", msg["type"], "trace_id", traceID)
	return nil
}

// --- Upstream command API ---

func friendlyError(err error) string {
	if err == nil {
		return "未知错误"
	}
	s := err.Error()
	if strings.Contains(s, "context deadline exceeded") || strings.Contains(s, "Client.Timeout") {
		return "命令服务响应超时，请稍后重试"
	}
	if strings.Contains(s, "no such host") || strings.Contains(s, "dial tcp") {
		return "无法连接命令服务，请检查网络后重试"
	}
	if strings.Contains(s, "upstream 5") {
		return "命令服务内部错误，请稍后重试"
	}
	return fmt.Sprintf("命令执行出错: %v", err)
}

func executeCommandServiceCommand(ctx context.Context, command string) (*CommandResult, error) {
	payload, _ := json.Marshal(map[string]string{"command": command})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.CommandAPIBaseURL+"/command", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := commandClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result CommandResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func resolveReply(result *CommandResult) Reply {
	if result == nil {
		return Reply{Text: "命令服务返回为空"}
	}
	content := strings.TrimSpace(result.Content)
	if content == "" {
		return Reply{Text: "命令服务返回为空"}
	}

	switch result.Type {
	case "image", "video", "file":
		if strings.HasPrefix(content, "http://") || strings.HasPrefix(content, "https://") {
			return Reply{MsgType: result.Type, MediaURL: content, MediaName: result.Name}
		}
		if strings.HasPrefix(content, "data:") {
			return Reply{MsgType: result.Type, MediaBase64: content, MediaName: result.Name}
		}
		return Reply{Text: fmt.Sprintf("命令返回了%s内容，但格式无法识别。", result.Type)}
	default:
		return Reply{Text: content}
	}
}

func writeSyncReply(w http.ResponseWriter, reply Reply) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]string{}
	if reply.MediaURL != "" {
		resp["reply_type"] = reply.MsgType
		resp["reply_url"] = reply.MediaURL
	} else if reply.MediaBase64 != "" {
		resp["reply_type"] = reply.MsgType
		resp["reply_base64"] = reply.MediaBase64
	}
	if reply.MediaName != "" {
		resp["reply_name"] = reply.MediaName
	}
	if reply.Text != "" {
		resp["reply"] = reply.Text
	}
	json.NewEncoder(w).Encode(resp)
}

// --- DB helpers ---

func getInstallation(id string) *Installation {
	inst := &Installation{}
	err := db.QueryRow("SELECT id, app_token, webhook_secret, bot_id FROM installations WHERE id=?", id).
		Scan(&inst.ID, &inst.AppToken, &inst.WebhookSecret, &inst.BotID)
	if err != nil {
		if err != sql.ErrNoRows {
			slog.Error("db query installation failed", "installation_id", id, "err", err)
		}
		return nil
	}
	return inst
}

func computeSignature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + ":"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// --- Utilities ---

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func rctx() context.Context {
	return context.Background()
}
