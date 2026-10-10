# Releasing to the CLIProxyAPI plugin store

The plugin is distributed through a **custom store source**: a `registry.json`
served from this repo's `main` branch plus **GitHub Releases** holding the
binaries. CLIProxyAPI's store page reads the registry, asks the GitHub API for
the repository's *latest* release, downloads
`neuralwatt_<version>_linux_<arch>.zip` and `checksums.txt`, verifies the
sha256, and installs `<plugins.dir>/linux/<arch>/neuralwatt-v<version>.so`.

Everything below was verified against CLIProxyAPI `v8.0.22`
(`internal/pluginstore/{registry,github,install,checksum,version}.go` and
`internal/api/handlers/management/plugin_store.go`).

## One-time: host config

Add the registry URL under `plugins.store-sources` (a list of registry URLs;
the official source is always included in front of it):

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/dangercharlie/cliproxy-neuralwatt/main/store/registry.json"
```

Pick the URL once and keep it. The host derives the source id from
`sha256(url)` and records it in the installed plugin's config; changing the
URL later makes the installed plugin's source "unknown" and blocks updates
until the plugin is uninstalled (`plugin_store.go` `pluginStoreInstallSourceStatus`).

## Per release

1. **Bump the version in Go first.** `Version` in
   `internal/plugin/manager.go` must equal the release version (`0.1.0` for
   tag `v0.1.0`). The store page reports the *installed* version from the
   loaded plugin's metadata and shows "update available" when it is lower
   than the latest release tag; a stale constant means a perpetual update
   prompt. Commit and push `main` (the registry and logo are fetched from
   `main`).

2. **Build on the VM** (done elsewhere; `scripts/build-linux.sh [amd64|arm64]`)
   and copy the result into this checkout as `dist/linux/<arch>/neuralwatt.so`.
   Only `linux/amd64` is needed for the Docker host; add `linux/arm64` if
   you built it, the packager picks up every `dist/linux/<arch>/` present.

3. **Package.**

   ```sh
   scripts/package-release.sh 0.1.0
   ```

   Produces, in `dist/release/`:

   ```text
   neuralwatt_0.1.0_linux_amd64.zip          # exactly one entry: neuralwatt.so (0755) at the zip root
   neuralwatt_0.1.0_linux_amd64.zip.sha256   # "<sha256>  neuralwatt_0.1.0_linux_amd64.zip"
   checksums.txt                             # all .sha256 lines, sorted (sha256sum format)
   ```

4. **Publish the release.** Tag `v<version>`, assets named with the bare
   version, not a draft, not a pre-release:

   ```sh
   gh release create v0.1.0 \
     dist/release/neuralwatt_0.1.0_linux_amd64.zip \
     dist/release/checksums.txt \
     --repo dangercharlie/cliproxy-neuralwatt \
     --target main \
     --title "v0.1.0" \
     --notes "Initial quota-only Neuralwatt plugin."
   ```

   `gh release create` creates the tag on `--target` if it does not exist.
   If you tagged locally first, `git push origin v0.1.0` and add
   `--verify-tag`. Add `dist/release/*.zip.sha256` to the upload if you like;
   the host ignores them and reads only `checksums.txt`.

5. **Update `store/registry.json` `version`** to the new number and push
   `main`. This is optional but recommended: for a plugin that is *not yet
   installed* the store page does not call GitHub and shows this value as the
   version; once installed, the latest release tag is used instead.

6. **Install / update from the UI.** Open the plugin store page; the plugin
   appears under a source named `raw.githubusercontent.com` (the host names
   custom sources by URL host). Install downloads the zip and `checksums.txt`,
   verifies the sha256, writes `<plugins.dir>/linux/amd64/neuralwatt-v0.1.0.so`,
   and sets in `config.yaml`:

   ```yaml
   plugins:
     configs:
       neuralwatt:
         enabled: true
         store:
           id: neuralwatt
           version: 0.1.0
           release-tag: v0.1.0
           repository: https://github.com/dangercharlie/cliproxy-neuralwatt
           source-id: source-<12 hex chars>
           source-url: https://raw.githubusercontent.com/dangercharlie/cliproxy-neuralwatt/main/store/registry.json
           install: { type: github-release }
   ```

   Your existing `api-key`, `label`, etc. under `plugins.configs.neuralwatt`
   are preserved (the handler only sets `enabled` and `store`).

## What the host requires (and where it is enforced)

| Rule | Evidence (CLIProxyAPI v8.0.22) |
|---|---|
| Release tag is `v<version>`; `v`/`V` is stripped; result must match `^[0-9][0-9A-Za-z.+-]*$` | `internal/pluginstore/github.go:116-122` `ReleaseVersion`, `registry.go:25,407-409` |
| Asset name is exactly `<id>_<version>_<goos>_<goarch>.zip` (bare version) | `github.go:351-359` `ArchiveName`, selected in `github.go:330-349` |
| A release asset literally named `checksums.txt` must exist | `github.go:338,345-347` |
| `checksums.txt` lines are `<sha256-hex>  <asset name>` (any whitespace, optional leading `*`, `#` comments ok) | `checksum.go:10-32` `ParseChecksums`, verified in `install.go:133` |
| Zip must contain exactly one dynamic library, named `neuralwatt.so` (or `neuralwatt-v<version>.so`), at the zip root | `install.go:318-348` `readTargetLibrary` |
| Installed as `<plugins.dir>/<goos>/<goarch>/neuralwatt-v<version>.so` | `install.go:310-316,371-373` |
| Latest version comes from `GET https://api.github.com/repos/<owner>/<repo>/releases/latest` | `github.go:66-85` |
| Pinned installs try both `0.1.0` and `v0.1.0` as tags | `plugin_store.go:471-480` |
| Version comparison is numeric per dotted segment; non-numeric segments make any difference count as an update | `version.go:13-69` |

## Gotchas

- **`releases/latest` ignores drafts and pre-releases.** Publish a normal
  release or the store will keep offering the previous one. GitHub picks
  "latest" by the tagged commit's date, so re-tagging an old commit can be
  overlooked; tag the tip of `main`.
- **Tag `v0.1.0`, assets `0.1.0`.** A tag without `v` also works (the host
  only strips it), but assets must never carry the `v`.
- **Plugin id = library name.** The registry `id` (`neuralwatt`), the file in
  the zip (`neuralwatt.so`) and `PluginID` in `manager.go` must all agree;
  the host derives the plugin id from the installed filename.
- **Remove the hand-copied `neuralwatt.so`** from `<plugins.dir>/linux/amd64/`
  after the first store install. The host prefers a versioned file over an
  unversioned one for the same id (`pluginhost/platform.go:204-216`) and,
  once `store.version` is recorded in config, loads only that exact version
  (`platform.go:166-173`), so the old file is dead weight at best and a
  silent "nothing loads" at worst if the versions drift.
- **GitHub API rate limits.** Metadata calls go to `api.github.com`
  (60 req/h unauthenticated per egress IP). The host caches the latest
  version for 1 h (failures 30 s) and only polls repos of *installed*
  plugins when listing the store. On a 403/429 it backs off (1 min doubling
  to 1 h) and the UI shows `plugin_store_rate_limited`. To authenticate,
  either set `GITHUB_TOKEN` in the container environment
  (`internal/githubauth/token.go:24`) or add a `plugins.store-auth` rule:

  ```yaml
  plugins:
    store-auth:
      - match: "https://api.github.com/repos/dangercharlie/"
        apply-to: ["metadata"]
        type: "github-token"
        token-env: "CLIPROXY_PLUGIN_GITHUB_TOKEN"
  ```

  Asset downloads use `browser_download_url` on `github.com`, which is not
  API-rate-limited (unless an auth rule matches the asset API URL, in which
  case the API URL is used instead).
- **Registry URL must return raw JSON.** Any `http(s)` URL works (the host
  follows up to 10 redirects and does not require `raw.githubusercontent.com`),
  but a `github.com/.../blob/...` page returns HTML and fails with
  `decode registry`. Raw GitHub content is CDN-cached for a few minutes, so a
  registry edit can take ~5 min to show up.
- **Registry validation is strict:** `schema_version` must be `1` (or `2`,
  only needed for `direct` installs); `id`, `name`, `description`, `author`,
  `repository` are required; `repository` must be exactly
  `https://github.com/<owner>/<repo>` (no `.git`, query or fragment);
  `version`, if present, must not start with `v`. A single invalid entry
  rejects the whole registry (`registry.go:166-230`).
- **`auth_required`** in the registry is display-only: it is passed straight
  to the store page together with `auth_configured` (whether a `store-auth`
  rule matches the source URL or the repo's releases API). The installer
  never gates on it. Leave it out for a public repo.
- **The `.so` is only reloaded by the host's own plugin hot-reload / restart
  logic.** The install response returns `restart_required: false` on Linux;
  `pluginhost/host.go:350-410` logs `plugin hot reloaded` when it swaps a
  versioned file in. If the new version does not show up as registered,
  restart the container.
- **Local check before publishing.** `unzip -Z <zip>` must list exactly one
  entry, `neuralwatt.so`, and `shasum -a 256 -c dist/release/checksums.txt`
  must pass (the packager already does both).
