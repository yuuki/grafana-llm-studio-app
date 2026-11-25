# Multi-Architecture Release Packaging, Plugin Signing & Catalog Submission Guide

This guide covers building multi-architecture release artifacts for `yuuki-llmstudio-app`, signing the plugin with `@grafana/sign-plugin` (Private or Community signature), submitting to the Grafana Plugin Catalog, and running the automated GitHub Actions release workflow so production Grafana environments **do not require `allow_loading_unsigned_plugins`**.

## 1. Signature Levels Overview

By default, Grafana refuses to load unsigned backend plugins unless the plugin ID is explicitly listed in `allow_loading_unsigned_plugins`. Signing generates a PGP-signed `MANIFEST.txt` inside the plugin directory containing SHA-256 digests of every distributed file. Grafana verifies `MANIFEST.txt` against its **built-in Grafana Labs public key** — meaning signed plugins also verify cleanly in **air-gapped / offline environments** with `public_key_retrieval_disabled = true`.

| Signature Type | Use Case | Requires `rootUrls`? | Requires `allow_loading_unsigned_plugins`? | Review Required? |
|---|---|---|---|---|
| **Unsigned** | Local development, CI, and first-time catalog submission before review approval | No | Yes (`yuuki-llmstudio-app`) | No |
| **Private** | Self-hosted / air-gapped Grafana instances owned by your organization | Yes (must match Grafana `root_url`) | **No** | No (instant self-service via Grafana Cloud Access Policy) |
| **Community** | Public OSS distribution via the Grafana Plugin Catalog for any Grafana instance | No | **No** | Yes (initial + update review by Grafana Labs) |

## 2. Prerequisites & Tooling

- **Pinned `@grafana/sign-plugin` (`3.3.3`):** Installed in `devDependencies` and exposed via `npm run sign`.
  > **Node.js Compatibility Note:** `@grafana/sign-plugin@3.3.3` supports Node.js `>=20` (matching this repository's Node.js 22 toolchain). `@grafana/sign-plugin@4.0.0+` requires Node.js `>=24.15.0` — do not upgrade to v4 until the project's Node runtime is upgraded accordingly.
- **Grafana Cloud Account & Organization Slug:**
  - The first segment of `id` in `src/plugin.json` (`yuuki` in `yuuki-llmstudio-app`) must match the Grafana Cloud organization slug that generates the Access Policy token.
  - Organizations re-packaging the plugin internally for Private signing under their own Grafana Cloud account must align the plugin ID prefix with their own org slug.
- **Grafana Cloud Access Policy Token (`GRAFANA_ACCESS_POLICY_TOKEN`):**
  1. Sign in to Grafana Cloud and open **My Account → Security → Access Policies**.
  2. Click **Create access policy**:
     - **Realm:** `<YOUR_ORG_SLUG> (all-stacks)`
     - **Scope:** `plugins:write`
  3. Create a token and store it securely (as an environment variable locally or as a GitHub Actions Repository Secret named `GRAFANA_ACCESS_POLICY_TOKEN`).
  4. Never use the deprecated `GRAFANA_API_KEY` variable or commit tokens to Git.

## 3. Building Multi-Architecture Release Artifacts Locally

The Go backend is cross-compiled with `CGO_ENABLED=0`, `-trimpath`, `-tags arrow_json_stdlib`, embedded `grafana-plugin-sdk-go/build.buildInfoJSON`, and `0755` executable permissions for all 5 targets:

- `gpx_yuuki_llmstudio_app_linux_amd64` (`linux/amd64`)
- `gpx_yuuki_llmstudio_app_linux_arm64` (`linux/arm64`)
- `gpx_yuuki_llmstudio_app_darwin_amd64` (`darwin/amd64`)
- `gpx_yuuki_llmstudio_app_darwin_arm64` (`darwin/arm64`)
- `gpx_yuuki_llmstudio_app_windows_amd64.exe` (`windows/amd64`)

### Unsigned Local Packaging (Development / Initial Catalog Submission)

```bash
npm ci
make package-airgap
```

This builds the frontend bundle, cross-compiles all 5 Go backend binaries, verifies architecture headers (`ELF`/`Mach-O`/`PE`) and `0755` permissions, and writes to `release/`:

- `yuuki-llmstudio-app-<version>.zip` (canonical Grafana archive with top-level `yuuki-llmstudio-app/` folder)
- `yuuki-llmstudio-app-<version>.tar.gz` (same layout for Unix/container ops)
- `yuuki-llmstudio-app-<version>.zip.sha1` (for Grafana.com catalog submission form)
- `yuuki-llmstudio-app-<version>-airgap.tar.gz` (optional offline bundle with archives, schemas, hardened `grafana.ini` template, docs, and `BUNDLE-MANIFEST.json`)
- `SHA256SUMS` (verifiable with `cd release && sha256sum -c SHA256SUMS`)

## 4. Signing Workflows

> **Important:** Always build **both** the frontend bundle and **all 5 Go backend binaries** (with `0755` permissions) **before** running `@grafana/sign-plugin`. Never add, modify, or remove files inside `dist/` after `MANIFEST.txt` is created, or Grafana will reject the plugin with a `Modified signature` error.

### 4.1 Private Signing (Self-Hosted & Air-Gapped Production)

Use Private signing when deploying to known Grafana instance URL(s) without waiting for Grafana Plugin Catalog publication:

```bash
export GRAFANA_ACCESS_POLICY_TOKEN="<your-access-policy-token>"
export GRAFANA_PLUGIN_ROOT_URLS="https://grafana.internal.example.com"

make build-frontend build-backend-all
node scripts/package-release.mjs --sign private --airgap
node scripts/verify-release.mjs --require-signed --airgap
```

Or sign `dist/` directly via npm:

```bash
export GRAFANA_ACCESS_POLICY_TOKEN="<your-access-policy-token>"
npm run sign -- --rootUrls https://grafana.internal.example.com,https://grafana-staging.internal.example.com
```

Notes:
- `--rootUrls` accepts a comma-separated list of URLs matching each Grafana server's configured `root_url` (including sub-path if `serve_from_sub_path = true`, e.g., `https://example.com/grafana`).
- Signing happens once on an internet-connected build/staging host (using `HTTPS_PROXY` if behind a corporate proxy). Transfer the resulting signed archive into the air-gapped network and remove `allow_loading_unsigned_plugins = yuuki-llmstudio-app` from `grafana.ini` while keeping `public_key_retrieval_disabled = true`.

### 4.2 Community Signing (Public Grafana Plugin Catalog)

Community signing is enabled by Grafana Labs after your initial catalog submission passes review:

1. **Pre-Submission Validation (Run Locally Before Submitting):**
   - Ensure `package.json` (`version`), `src/plugin.json` (`info.version`, `info.updated`, `info.links`, `info.author`), `README.md`, `LICENSE`, and `CHANGELOG.md` are up to date.
   - Before submitting to the public catalog, capture UI screenshots (Studio generation + dry-run preview + App Config) under `src/img/` and reference them in `src/plugin.json` `info.screenshots` for catalog discoverability.
   - Run Grafana's official `plugin-validator-cli` against the packaged ZIP and source tree:
     ```bash
     docker run --rm \
       -v "${PWD}/release:/release:ro" \
       -v "${PWD}:/src:ro" \
       grafana/plugin-validator-cli \
       -sourceCodeUri file:///src \
       /release/yuuki-llmstudio-app-0.1.0.zip
     ```
2. **Initial Submission (Unsigned Archive):**
   - Sign in to Grafana Cloud as an organization admin → **Org Settings → My Plugins → Submit New Plugin**.
   - Choose **Public (free)**.
   - Provide:
     - **Plugin (zip file):** GitHub Release URL for `yuuki-llmstudio-app-<version>.zip`
     - **MD5 or SHA1:** GitHub Release URL for `yuuki-llmstudio-app-<version>.zip.sha1`
     - **Source code:** Public Git repository URL pinned to the matching release tag (e.g., `https://github.com/yuuki/grafana-llm-studio-app/tree/v0.1.0`)
     - **Testing guidance:** Reference `deploy/docker-compose.yml` (`make verify` + mock mode quick start in `README.md`), noting that provisioning is included in `deploy/grafana/provisioning/`.
   - First-time submissions are expected to be unsigned until Grafana Labs assigns the `community` signature level.
3. **Signing Approved Releases (`community`):**
   - Once Grafana Labs grants the `community` signature level, sign subsequent releases without `--rootUrls`:
     ```bash
     export GRAFANA_ACCESS_POLICY_TOKEN="<your-access-policy-token>"
     make build-frontend build-backend-all
     node scripts/package-release.mjs --sign community --require-signed --airgap
     node scripts/verify-release.mjs --require-signed --airgap
     ```
   - Submit updates via **Org Settings → My Plugins → Submit Update** (selecting **Single** archive since all 5 architectures are bundled inside `yuuki-llmstudio-app-<version>.zip`).

## 5. Automated GitHub Actions Release Workflow (`.github/workflows/release.yml`)

### Repository Configuration

Configure under **GitHub Repository → Settings → Secrets and variables → Actions**:

| Kind | Name | Required? | Purpose |
|---|---|---|---|
| **Secret** | `GRAFANA_ACCESS_POLICY_TOKEN` | Required for signed releases | Grafana Cloud Access Policy token (`plugins:write`) used by `@grafana/sign-plugin` |
| **Variable** | `GRAFANA_PLUGIN_SIGNING_MODE` | Optional (default: `auto`) | One of `auto`, `community`, `private`, `off` |
| **Variable** | `GRAFANA_PLUGIN_ROOT_URLS` | Required only for `private` signing | Comma-separated Grafana `root_url` values passed to `--rootUrls` |
| **Variable** | `GRAFANA_REQUIRE_SIGNED_RELEASE` | Optional (default: `false`) | Set to `true` once Community/Private signing is active so CI fails loudly instead of ever publishing an unsigned release |
| **Variable** | `BUILD_AIRGAP_BUNDLE` | Optional (default: `true`) | Set to `false` to skip building `yuuki-llmstudio-app-<version>-airgap.tar.gz` on tags |

### Cutting a Release

1. Update version numbers in lockstep:
   - `package.json` (`"version": "X.Y.Z"`)
   - `src/plugin.json` (`"info.version": "X.Y.Z"` and `"info.updated": "YYYY-MM-DD"`)
   - `CHANGELOG.md` (add a dated `## [X.Y.Z] - YYYY-MM-DD` section)
2. Merge the release preparation PR to `main` and confirm CI (`verify` + Grafana `13.3.1` / `12.4.15` E2E) is green.
3. Create and push the matching annotated Git tag:
   ```bash
   git tag -a vX.Y.Z -m "Release vX.Y.Z"
   git push origin vX.Y.Z
   ```
4. `.github/workflows/release.yml` automatically:
   - Verifies tag `vX.Y.Z` matches `package.json` and `src/plugin.json`, and runs all schema, typecheck, Jest, release-helper, bundle, Go vet, and Go race checks.
   - Cross-compiles all 5 Go backend architectures in parallel (`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`).
   - Signs `dist/` via `@grafana/sign-plugin` (when configured) and strictly validates `MANIFEST.txt` file hashes.
   - Packages `.zip`, `.tar.gz`, `.zip.sha1`, optional `-airgap.tar.gz`, and `SHA256SUMS`.
   - Verifies every archive, every binary architecture header (`ELF`/`Mach-O`/`PE`), `0755` permissions, and `sha256sum -c SHA256SUMS`.
   - Runs `grafana/plugin-validator-cli` and publishes the findings to the workflow Job Summary.
   - Publishes the GitHub Release with release notes, signing status notice, and checksums (marking `-rc` / prerelease tags as prereleases automatically).
5. **Dry-Run Testing Without Tagging:** Trigger **Actions → Release → Run workflow** (`workflow_dispatch`) with `publish_release: false` to test full cross-compilation, signing, packaging, and `plugin-validator-cli` output on any branch.

## 6. Troubleshooting

- **`Field is required: rootUrls` when running Community signing:** Grafana Labs has not yet assigned a `community` signature level to `yuuki-llmstudio-app`. Submit an unsigned build first (Section 4.2) or use Private signing with `--rootUrls`.
- **`Modified signature` error in Grafana:** A file in `dist/` was modified, added, or removed after `MANIFEST.txt` was generated, or `MANIFEST.txt` was generated on Windows with `\\` path separators. Always sign on Linux (or inside `.github/workflows/release.yml`) as the final step before zipping, and verify with `node scripts/verify-release.mjs --require-signed`.
- **Signature still rejected offline:** Ensure `yuuki-llmstudio-app` is extracted intact with `MANIFEST.txt` at `/var/lib/grafana/plugins/yuuki-llmstudio-app/MANIFEST.txt`, and for Private signatures verify Grafana's `[server] root_url` matches one of the signed `--rootUrls` exactly (scheme, host, port, and sub-path).
