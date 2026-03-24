# openilink-app-command-service

OpenILink Hub App for a command service, currently backed by `bhwa233-api`.

## What it does

- Dynamically fetches the upstream command list from `GET /command/hp`
- Registers those commands directly in the App manifest
- Forwards `/<command> <text>` to `POST /command` on `bhwa233-api`
- Avoids the old two-layer command style like `/command-service hp`

Examples:

- `/hp`
- `/wb`
- `/gold`
- `/a 你好鸡哥`
- `/gi 夜晚城市霓虹街道`

## Current behavior

- Text responses are returned directly
- If the upstream service returns an image result, this App currently falls back to a text notice
- This keeps the App aligned with the current App skill guidance, which documents media reply support as future-facing

## App manifest

The app serves its manifest at:

- `GET /manifest.json`

Manifest values:

- Slug: `command-service`
- Name: `Command Service`
- Commands: dynamically generated from upstream `/command/hp`

## Environment

- `PORT` — listen port, default `8081`
- `HUB_URL` — Hub origin, default `https://hub.openilink.com`
- `BASE_URL` — public base URL of this app
- `DATABASE_URL` — Postgres DSN for installation storage
- `COMMAND_API_BASE_URL` — upstream API base URL, default `https://bhwa233-api.vercel.app/api`
- `COMMAND_API_TIMEOUT_MS` — upstream timeout in milliseconds, default `2500`

## Local run

```bash
go mod tidy
go run .
```

## Docker

```bash
docker compose up -d --build
```
