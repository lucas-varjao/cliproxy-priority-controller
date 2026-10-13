# CLIProxyAPI Priority Controller

A small companion service for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) that gives new sessions to the OAuth account whose **weekly quota resets soonest**.

The goal is to consume quota that will become available again sooner before consuming quota with a later reset, while leaving CLIProxyAPI's own session affinity in charge of already-bound sessions.

## Why this exists

CLIProxyAPI supports per-credential `priority`, and higher values are preferred by its scheduler. It also supports session affinity. What it does not currently provide as a built-in routing strategy is an `earliest-weekly-reset-first` policy.

This controller fills that gap without replacing CLIProxyAPI's scheduler:

1. Lists Claude and Codex OAuth credentials through the Management API.
2. Uses CLIProxyAPI's authenticated `api-call` proxy to query each provider's read-only usage endpoint.
3. Reads both the short/session window (normally 5 hours) and the weekly window when available.
4. Sorts accounts by weekly reset time, soonest first.
5. Optionally applies a 5-hour guardrail so nearly exhausted short windows stop receiving new sessions.
6. Writes only the credential `priority` field, and only when it changed.

Existing session affinity remains inside CLIProxyAPI. The controller never chooses a credential for an inference request itself.

## Default policy: weekly reset first

By default, `CLIPROXY_FIVE_HOUR_MODE=off`, so the 5-hour window is observed but does **not** affect routing.

| Weekly reset | Priority |
| --- | ---: |
| Oct 14 | 100 |
| Oct 15 | 99 |
| Oct 16 | 98 |
| Oct 18 | 97 |

A new session goes to the highest available priority tier. When that account is exhausted, CLIProxyAPI's normal quota/cooldown behavior can fall through to the next available tier. After a weekly reset rolls forward, this controller recomputes the ordering.

Accounts with the exact same reset timestamp receive the same priority, so CLIProxyAPI can apply its configured routing strategy inside that tier.

## Optional 5-hour guardrail

The short window is useful as an **admission-control signal**. A credential whose 5-hour quota is almost exhausted is a poor place to start a new long-lived session, even when its weekly reset is the earliest.

Enable the guardrail with:

```bash
CLIPROXY_FIVE_HOUR_MODE=guardrail
CLIPROXY_FIVE_HOUR_MAX_UTILIZATION=90
```

With the default threshold, credentials at **90% used or higher** are temporarily placed behind healthy credentials. Weekly reset time still determines ordering inside the healthy group and inside the constrained group.

For example:

| Account | Weekly reset | 5h used | Result |
| --- | --- | ---: | --- |
| A | 1 day | 95% | constrained |
| B | 2 days | 25% | preferred |
| C | 3 days | 10% | next |

The resulting order is `B -> C -> A`. Existing sessions already bound to A are not moved by this controller. After A's short window resets and utilization drops below the threshold, it naturally returns to the position dictated by its weekly reset.

If every account is constrained, the controller still orders them by weekly reset, so the pool remains usable.

## Dry-run output

`--dry-run` logs the decision for every eligible credential, including accounts whose priority is already correct. This makes it useful for observing both windows before enabling the guardrail.

Typical fields include:

```text
provider=claude
credential=example.json
decision=weekly-reset
weekly_reset_at=2026-10-14T07:59:00Z
weekly_utilization=80
five_hour_reset_at=2026-10-13T01:29:00Z
five_hour_utilization=47
old_priority=90
new_priority=100
```

When the guardrail is responsible for the demotion, `decision=five-hour-guardrail`.

## Supported providers

- **Claude Code OAuth** via `GET https://api.anthropic.com/api/oauth/usage`
- **Codex / ChatGPT OAuth** via `GET https://chatgpt.com/backend-api/wham/usage`

These quota endpoints are not stable public provider APIs. The parser is intentionally small and defensive, but upstream schema changes can require updates.

For Claude, the controller reads the legacy `five_hour` / `seven_day` fields and can fall back to matching entries in the newer `limits` array.

For Codex, window duration is recognized using `limit_window_seconds` when present rather than assuming `primary_window` or `secondary_window` always means a fixed duration. Traditional duration-less primary/secondary responses are also supported.

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

To test the optional guardrail without applying it:

```bash
CLIPROXY_MANAGEMENT_KEY='your-management-key' \
./cliproxy-priority-controller \
  --five-hour-mode guardrail \
  --five-hour-max-utilization 90 \
  --dry-run --once
```

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
| `CLIPROXY_PRIORITY_MAX` | `100` | Priority assigned to the highest-ranked credential |
| `CLIPROXY_PRIORITY_STEP` | `1` | Priority decrement per distinct routing tier |
| `CLIPROXY_FIVE_HOUR_MODE` | `off` | `off` or `guardrail` |
| `CLIPROXY_FIVE_HOUR_MAX_UTILIZATION` | `90` | Percent used at which guardrail marks a credential constrained |
| `CLIPROXY_DRY_RUN` | `false` | Log decisions without applying changes |
| `CLIPROXY_ONCE` | `false` | Reconcile once and exit |

See [.env.example](.env.example) for a ready-to-copy example.

## Safety behavior

The controller deliberately favors consistency over partial reordering:

- Disabled credentials are ignored.
- If **any eligible account for a provider fails its quota read**, no priorities for that provider are changed during that cycle.
- On an upstream quota HTTP 401, the controller asks CLIProxyAPI to refresh that credential and retries once.
- Provider access tokens are never read by this process. The Management API substitutes `$TOKEN$` server-side for the quota request.
- Tokens and Management API keys are never logged.
- If a short-window reading is unavailable but the weekly reading is valid, the credential remains eligible and is ranked by weekly reset only.
- Exhausted/unavailable accounts are still ranked. CLIProxyAPI remains responsible for skipping credentials under cooldown; keeping their priority means they naturally become eligible again when the cooldown expires.

## Session affinity

This project is intentionally a **priority controller, not a scheduler plugin**. It does not intercept model requests and does not bypass CLIProxyAPI's selector.

That distinction matters when session affinity is enabled: changing credential priorities influences new-session/fallback selection, while an existing healthy session can remain bound to its current credential according to CLIProxyAPI's affinity rules.

The 5-hour guardrail therefore protects **new sessions** without deliberately migrating healthy existing sessions.

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
- Combining the 5-hour and weekly windows into an opaque weighted score.
- Implementing cooldown behavior that CLIProxyAPI already owns.

## License

MIT
