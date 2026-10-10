# cliproxy-neuralwatt

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) v8 native plugin
that makes [Neuralwatt](https://neuralwatt.com) quota visible wherever the
proxy's quota API is read (CLIProxyBar, management tools), and adds a single
summary endpoint plus a phone-friendly page covering every credential the
proxy holds (Claude, Codex, Neuralwatt).

It is **quota-only**: Neuralwatt traffic keeps flowing through the host's
`openai-compatibility` entry with the host's own format translation. The
plugin declares `auth_provider` + `quota_provider` + `management_api`, and no
executor.

## What it adds

| Surface | Path |
|---|---|
| Generic quota contract | `GET /v0/management/quota/providers`, `POST /v0/management/quota/fetch` (`auth_index` of the neuralwatt credential) |
| All-provider summary | `GET /v0/management/plugins/neuralwatt/summary` (`?refresh=1` shortens the 90 s cache to 20 s; a failed refresh serves the last good reading flagged `stale` for up to 15 min, a 429 backs that credential off for 2 to 15 min, and good readings persist to `cache-file` across restarts) |
| Raw upstream + mapping | `GET /v0/management/plugins/neuralwatt/upstream` (the host reserves `/quota` for its own per-plugin fetch, `GET /v0/management/plugins/neuralwatt/quota?auth_index=…`) |
| Status page | `GET /v0/resource/plugins/neuralwatt/status` (prompts once for the management key, stores it in localStorage) |

Neuralwatt mapping: the included-energy allowance becomes one bucket
(`remainingFraction`, reset at `kwh_reset_date`); credits remaining, month
spend, month requests and energy left become summary metrics; the plan name
becomes the subscription.

## Install

1. Build or download `neuralwatt.so` (linux/amd64 for the Docker image):
   `scripts/build-linux.sh`.
2. Copy it to `<plugins.dir>/linux/amd64/neuralwatt.so`. With the Docker
   image, mount a host directory at `/CLIProxyAPI/plugins`.
3. Configure:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    neuralwatt:
      enabled: true
      api-key: "sk-..."        # or api-key-env / api-key-file
      label: "Neuralwatt"
      # base-url: "https://api.neuralwatt.com"
      # cache-seconds: 90
      # timeout-seconds: 15
      # cache-file: "plugins/neuralwatt-cache.json"   # last good readings, survives restarts; "" disables
      # summary:
      #   providers: [claude, codex, neuralwatt]
```

4. Restart CLIProxyAPI. On start the plugin saves a `neuralwatt-<digest>.json`
   credential through the host (idempotent), which then shows up in
   auth-files with `supports_quota: true`.

## Develop

```sh
go test ./...
go vet ./...
scripts/build-linux.sh        # needs Docker
```

Requires Go 1.26+ and a C compiler. SDK pinned to CLIProxyAPI v8.0.22 to match
the deployed host; bump with the host.
