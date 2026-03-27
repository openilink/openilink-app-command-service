# openilink-app-command-service

OpenILink Hub App that proxies slash commands to an upstream command API (currently [bhwa233-api](https://github.com/lxw15337674/bhwa233-api)).

## What it does

- Forwards `/<command> <text>` to `POST /command` on the upstream API
- Supports text, image, video, and file replies (sync and async)
- On startup and after each OAuth install, syncs available commands to Hub as tools via `PUT /bot/v1/app/tools`
- Uses sync/async pattern: replies within 5s are returned synchronously, slower commands fall back to async push via Bot API

Examples:

- `/hp` — command help
- `/gold` — real-time gold price
- `/s 600519` — stock info
- `/a 你好鸡哥` — ask a question
- `/gi 夜晚城市霓虹街道` — Gemini image generation

## Install on Hub

### 1. Create the App

In Hub Dashboard -> Apps -> Create App:

| Field | Value |
|---|---|
| Name | Command Service |
| Slug | `command-service` |
| Scopes | `message:write`, `tools:write` |
| Events | `command` |
| Webhook URL | `https://<your-domain>/hub/webhook` |
| OAuth Setup URL | `https://<your-domain>/oauth/setup` |
| OAuth Redirect URL | `https://<your-domain>/oauth/callback` |

Tools will be auto-synced after installation — no need to add them manually.

### 2. Verify Webhook URL

Click "Verify URL" in the dashboard. The service will respond to the `url_verification` challenge automatically.

### 3. Install to a Bot

Click "Install" on a Bot. Hub will open an OAuth popup:

1. Hub redirects to `/oauth/setup` with `hub`, `app_id`, `bot_id`, `state`, `return_url`
2. Service generates PKCE challenge and redirects to Hub's authorize endpoint
3. Hub redirects back to `/oauth/callback` with a temporary `code`
4. Service exchanges `code` + `code_verifier` for `app_token` and `webhook_secret`
5. Credentials are stored in SQLite, popup closes automatically

After install, tools are automatically synced to Hub from the upstream `/command/hp` endpoint.

### 4. Reauthorize (if you add scopes later)

If you add new scopes to the App (e.g. `tools:write`), existing installations need reauthorization:

```
POST /api/apps/{id}/installations/{iid}/reauthorize
```

## Environment

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8081` | Listen port |
| `HUB_URL` | `https://hub.openilink.com` | Hub origin |
| `BASE_URL` | — | Public base URL of this service |
| `DB_PATH` | `/data/command-service.db` | SQLite database path |
| `APP_ID` | — | App ID (optional, Hub passes it via OAuth) |
| `COMMAND_API_BASE_URL` | `https://bhwa233-api.vercel.app/api` | Upstream command API |
| `COMMAND_API_TIMEOUT_MS` | `120000` | Upstream timeout (ms) |
| `SYNC_DEADLINE_MS` | `5000` | Max wait before falling back to async |

## Routes

| Method | Path | Description |
|---|---|---|
| `POST` | `/hub/webhook` | Hub event webhook |
| `GET` | `/oauth/setup` | OAuth PKCE setup (entry point) |
| `GET` | `/oauth/callback` | OAuth PKCE callback (code exchange) |
| `GET` | `/health` | Health check |

## Local development

```bash
export DB_PATH=./dev.db
go run .
```

## Docker

```bash
docker compose up -d --build
```

SQLite data is persisted in a Docker volume (`command-service-data` -> `/data/`).
