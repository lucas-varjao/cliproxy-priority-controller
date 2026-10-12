# CLIProxyAPI Priority Controller

A small companion service for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) that gives new sessions to the OAuth account whose **weekly quota resets soonest**.

The goal is to consume quota that will become available again sooner before consuming quota with a later reset, while leaving CLIProxyAPI's own session affinity in charge of already-bound sessions.

## Why this exists

CLIProxyAPI supports per-credential `priority`, and higher values are preferred by its scheduler. It also supports session affinity. What it does not currently provide as a built-in routing strategy is an `earliest-weekly-reset-first` policy.

This controller fills that gap without replacing CLIProxyAPI's scheduler:

1. Lists Claude and Codex OAuth credentials through the Management API.
2. Uses CLIProxyAPI's authenticated `api-call` proxy to query each provider's read-only usage endpoint.
3. Extracts the weekly reset time.
4. Sorts accounts by reset time, soonest first.
5. Writes only the credential `priority` field, and only when it changed.

Existing session affinity remains inside CLIProxyAPI. The controller never chooses a credential for an inference request itself.

## Example

| Weekly reset | Priority |
| --- | ---: |
| Oct 14 | 100 |
| Oct 15 | 99 |
| Oct 16 | 98 |
| Oct 18 | 97 |

A new session goes to the highest available priority tier. When that account is exhausted, CLIProxyAPI's normal quota/cooldown behavior can fall through to the next available tier. After a weekly reset rolls forward, this controller recomputes the ordering.

Accounts with the exact same reset timestamp receive the same priority, so CLIProxyAPI can apply its configured routing strategy inside that tier.

## Supported providers

- **Claude Code OAuth** via `GET https://api.anthropic.com/api/oauth/usage`
- **Codex / ChatGPT OAuth** via `GET https://chatgpt.com/backend-api/wham/usage`

These quota endpoints are not stable public provider APIs. The parser is intentionally small and defensive, but upstream schema changes can require updates.

For Codex, weekly windows are recognized using `limit_window_seconds` when present rather than assuming `primary_window` or `secondary_window` always means a fixed duration.

## Requirements

- CLIProxyAPI with its Management API enabled.
- Go 1.23+ if building from source.
- OAuth credentials for at least one supported provider.

CLIProxyAPI v8 Management API is preferred. `CLIPROXY_API_VERSION=auto` falls back to the older v0 routes only when v8 is unavailable.

## Quick start

```bash
git clone https://github.com/lucas-varjao/cliproxy-priority-controller.git
cd cliproxy-priority-controller

go build -o cliproxy-priority-controller ./cmd/cliproxy-priority-controller

CLIPROXY_MANAGEMENT_KEY='your-management-key' \
./cliproxy-priority-controller --dry-run --once
```

If the ordering looks correct:

```bash
CLIPROXY_MANAGEMENT_KEY='your-management-key' \
./cliproxy-priority-controller --once
```

Run without `--once` to reconcile continuously.

## Configuration

Environment variables and equivalent command-line flags are supported.

| Variable | Default | Purpose |
| --- | --- | --- |
| `CLIPROXY_URL` | `http://127.0.0.1:8317` | CLIProxyAPI server root |
| `CLIPROXY_MANAGEMENT_KEY` | empty | Management API bearer key |
| `MANAGEMENT_KEY` | empty | Fallback alias for the management key |
| `CLIPROXY_API_VERSION` | `auto` | `auto`, `v8`, or `v0` |
| `CLIPROXY_PROVIDERS` | `claude,codex` | Comma-separated providers to manage |
| `CLIPROXY_INTERVAL` | `5m` | Reconciliation interval |
| `CLIPROXY_HTTP_TIMEOUT` | `30s` | Per-request HTTP timeout |
| `CLIPROXY_PRIORITY_MAX` | `100` | Priority assigned to the soonest reset |
| `CLIPROXY_PRIORITY_STEP` | `1` | Priority decrement per distinct reset |
| `CLIPROXY_DRY_RUN` | `false` | Log changes without applying them |
| `CLIPROXY_ONCE` | `false` | Reconcile once and exit |

See [.env.example](.env.example) for a ready-to-copy example.

## Safety behavior

The controller deliberately favors consistency over partial reordering:

- Disabled credentials are ignored.
- If **any eligible account for a provider fails its quota read**, no priorities for that provider are changed during that cycle.
- On an upstream quota HTTP 401, the controller asks CLIProxyAPI to refresh that credential and retries once.
- Provider access tokens are never read by this process. The Management API substitutes `$TOKEN$` server-side for the quota request.
- Tokens and Management API keys are never logged.
- Exhausted/unavailable accounts are still ranked. CLIProxyAPI remains responsible for skipping credentials under cooldown; keeping their priority means they naturally become eligible again when the cooldown expires.

## Session affinity

This project is intentionally a **priority controller, not a scheduler plugin**. It does not intercept model requests and does not bypass CLIProxyAPI's selector.

That distinction matters when session affinity is enabled: changing credential priorities influences new-session/fallback selection, while an existing healthy session can remain bound to its current credential according to CLIProxyAPI's affinity rules.

## Docker

On Linux, host networking is the easiest way for the container to reach a CLIProxyAPI instance listening on `127.0.0.1:8317`:

```bash
cp .env.example .env
# Edit .env and set CLIPROXY_MANAGEMENT_KEY if required.

docker compose -f compose.example.yml up -d --build
```

If you do not use host networking, set `CLIPROXY_URL` to an address reachable from the container.

## systemd user service

Build/install the binary and copy the example unit:

```bash
mkdir -p ~/.local/bin ~/.config/cliproxy-priority-controller ~/.config/systemd/user
cp cliproxy-priority-controller ~/.local/bin/
cp deploy/cliproxy-priority-controller.service.example \
  ~/.config/systemd/user/cliproxy-priority-controller.service
cp .env.example ~/.config/cliproxy-priority-controller/env
chmod 600 ~/.config/cliproxy-priority-controller/env

systemctl --user daemon-reload
systemctl --user enable --now cliproxy-priority-controller
```

## Development

```bash
gofmt -w .
go test ./...
go vet ./...
```

The project uses only the Go standard library.

## Non-goals

- Replacing CLIProxyAPI's session affinity.
- Persisting provider tokens locally.
- Predicting quota consumption.
- Routing by the 5-hour window. Ordering is intentionally based on the weekly reset.
- Implementing cooldown behavior that CLIProxyAPI already owns.

## License

MIT
