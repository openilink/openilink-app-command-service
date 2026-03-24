# e2e

This directory stores mock E2E fixtures and helper files for `openilink-app-command-service`.

## Scope

These tests deliberately avoid real Hub installation and real Postgres startup.
Instead they validate the end-to-end command flow with:

- a mocked upstream command API
- mocked installation lookup via `sqlmock`
- signed webhook envelopes hitting the real handler path

## How to run

```bash
go test -run 'TestMock' -v ./...
```

## Fixtures

- `fixtures/commands_hp.json` — sample upstream command list used by mock E2E tests
