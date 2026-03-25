package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

type Config struct {
	Port                string
	HubURL              string
	DatabaseURL         string
	BaseURL             string
	CommandAPIBaseURL   string
	CommandAPITimeoutMS int
	SyncDeadlineMS      int
}

type Installation struct {
	ID            string
	AppToken      string
	SigningSecret string
	BotID         string
	Handle        string
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
}

// Reply represents a resolved reply ready for sync or async delivery.
type Reply struct {
	Text        string // text content or fallback
	MsgType     string // "text" or "image"
	MediaURL    string // non-empty for image URL replies
	MediaBase64 string // non-empty for base64 image replies
}

type CommandDefinition struct {
	Key         string `json:"key"`
	Description string `json:"description"`
	Type        string `json:"type,omitempty"`
}

var (
	cfg        Config
	db         *sql.DB
	httpClient *http.Client
)

func main() {
	cfg = Config{
		Port:                envOr("PORT", "8081"),
		HubURL:              strings.TrimRight(envOr("HUB_URL", "https://hub.openilink.com"), "/"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		BaseURL:             strings.TrimRight(os.Getenv("BASE_URL"), "/"),
		CommandAPIBaseURL:   strings.TrimRight(envOr("COMMAND_API_BASE_URL", "https://bhwa233-api.vercel.app/api"), "/"),
		CommandAPITimeoutMS: envIntOr("COMMAND_API_TIMEOUT_MS", 60000),
		SyncDeadlineMS:      envIntOr("SYNC_DEADLINE_MS", 2000),
	}

	httpClient = &http.Client{Timeout: time.Duration(cfg.CommandAPITimeoutMS) * time.Millisecond}

	var err error
	db, err = sql.Open("postgres", cfg.DatabaseURL)
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
	mux.HandleFunc("POST /callback", handleCallback)
	mux.HandleFunc("GET /manifest.json", handleManifest)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	addr := ":" + cfg.Port
	slog.Info("command service app starting", "addr", addr, "hub", cfg.HubURL, "command_api", cfg.CommandAPIBaseURL)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func migrate() error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS installations (
		id TEXT PRIMARY KEY,
		app_token TEXT NOT NULL,
		signing_secret TEXT NOT NULL,
		bot_id TEXT NOT NULL,
		handle TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	return err
}

func handleCallback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InstallationID string `json:"installation_id"`
		AppToken       string `json:"app_token"`
		SigningSecret  string `json:"signing_secret"`
		BotID          string `json:"bot_id"`
		Handle         string `json:"handle"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("callback: invalid json", "err", err)
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	slog.Info("callback received", "installation_id", req.InstallationID, "bot_id", req.BotID, "handle", req.Handle)

	_, err := db.Exec(`INSERT INTO installations (id, app_token, signing_secret, bot_id, handle)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET app_token=$2, signing_secret=$3, bot_id=$4, handle=$5`,
		req.InstallationID, req.AppToken, req.SigningSecret, req.BotID, req.Handle)
	if err != nil {
		slog.Error("save installation failed", "installation_id", req.InstallationID, "err", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}

	slog.Info("installation saved", "installation_id", req.InstallationID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"request_url": cfg.BaseURL + "/hub/webhook"})
}

func handleManifest(w http.ResponseWriter, r *http.Request) {
	defs, err := fetchCommandDefinitions(r.Context())
	if err != nil {
		slog.Warn("fetch command definitions failed", "err", err)
		defs = fallbackCommandDefinitions()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"slug":         "command-service",
		"name":         "Command Service",
		"description":  "Expose upstream command-service commands directly, currently backed by bhwa233-api",
		"icon":         "🛰️",
		"tools":        buildManifestTools(defs),
		"events":       []string{},
		"scopes":       []string{"messages.send"},
		"redirect_url": cfg.BaseURL + "/callback",
	})
}

func buildManifestTools(defs []CommandDefinition) []map[string]any {
	tools := make([]map[string]any, 0, len(defs))
	seen := map[string]bool{}

	for _, def := range defs {
		key := strings.TrimSpace(def.Key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true

		desc := strings.TrimSpace(def.Description)
		if desc == "" {
			desc = "Run command: " + key
		}

		tool := map[string]any{
			"name":        key,
			"description": desc,
			"command":     key,
		}

		if strings.HasSuffix(def.Key, " ") {
			tool["parameters"] = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "Command arguments",
					},
				},
				"required": []string{"text"},
			}
		}

		tools = append(tools, tool)
	}

	sort.Slice(tools, func(i, j int) bool {
		return tools[i]["name"].(string) < tools[j]["name"].(string)
	})
	return tools
}

func fetchCommandDefinitions(ctx context.Context) ([]CommandDefinition, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.CommandAPIBaseURL+"/command/hp", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var defs []CommandDefinition
	if err := json.Unmarshal(body, &defs); err != nil {
		return nil, err
	}
	return defs, nil
}

func fallbackCommandDefinitions() []CommandDefinition {
	return []CommandDefinition{{Key: "hp", Description: "hp - 获取命令帮助"}}
}

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
	expected := computeSignature(inst.SigningSecret, timestamp, body)
	if signature != "sha256="+expected {
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

	// Prefer structured args from AI Agent, fall back to free-form text from user.
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
		// Finished within sync deadline.
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
		asyncResult, asyncErr := executeCommandServiceCommand(rctx(), fullCommand)
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
	} else if reply.MediaBase64 != "" {
		msg["type"] = reply.MsgType
		msg["base64"] = reply.MediaBase64
	} else {
		msg["type"] = "text"
		msg["content"] = reply.Text
	}

	payload, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(rctx(), http.MethodPost, cfg.HubURL+"/bot/v1/messages/send", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+appToken)
	req.Header.Set("Content-Type", "application/json")
	if traceID != "" {
		req.Header.Set("X-Trace-Id", traceID)
	}

	resp, err := httpClient.Do(req)
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

	resp, err := httpClient.Do(req)
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
	case "image":
		if strings.HasPrefix(content, "http://") || strings.HasPrefix(content, "https://") {
			return Reply{MsgType: "image", MediaURL: content}
		}
		if strings.HasPrefix(content, "data:image/") {
			return Reply{MsgType: "image", MediaBase64: content}
		}
		return Reply{Text: "命令返回了图片内容，但格式无法识别，御坂如实地报告道。"}
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
	if reply.Text != "" {
		resp["reply"] = reply.Text
	}
	json.NewEncoder(w).Encode(resp)
}

func getInstallation(id string) *Installation {
	inst := &Installation{}
	err := db.QueryRow("SELECT id, app_token, signing_secret, bot_id, handle FROM installations WHERE id=$1", id).
		Scan(&inst.ID, &inst.AppToken, &inst.SigningSecret, &inst.BotID, &inst.Handle)
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
