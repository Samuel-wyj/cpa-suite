# CPA Suite

[中文](README.md)

CPA Suite is an all-in-one management plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA): API key billing and plan quotas, concurrency and routing rules, client restrictions, quota forecasting, usage ranking, degradation monitoring and an alert center, in one plugin with a UI matching the CPA management panel.

## Origin and license

CPA Suite is a modified and extended version of [haowang02/cpa-plugin-key-billing](https://github.com/haowang02/cpa-plugin-key-billing) (cpa-key-billing) v1.3.20, released under the MIT License, copyright Hao Wang. CPA Suite is also MIT licensed; see [LICENSE](LICENSE). Changes to upstream are listed in [docs/UPSTREAM_PATCHES.md](docs/UPSTREAM_PATCHES.md). Thanks to the original author.

## Features

- **Billing and plans** per API key (inherited from cpa-key-billing).
- **Routing and concurrency**: per-key allowed credentials and order, model-aware selection, slot management.
- **Client restrictions** by User-Agent and other rules (one-time import from client-guard).
- **Quota forecast and usage ranking** for Codex and Grok accounts.
- **Degradation monitor and alert center** with paging, filters, grouping, bulk actions, export and configurable retention.
- **Self-service usage** lookup for key holders (cc-switch usage format).

## Requirements

CLIProxyAPI v8.0.44 or newer (verified on v8.0.48 and v8.0.49). Platforms: linux amd64/arm64, macOS amd64/arm64, windows amd64.

## Install

Search `cpa-suite` in the plugin store of the CPA management panel (CPAMC or CPAMP) and restart CPA. Or download the zip for your platform from [Releases](https://github.com/Samuel-wyj/cpa-suite/releases), verify it with `checksums.txt`, put `cpa-suite.so` / `.dylib` / `.dll` into CPA's `plugins/` directory and restart.

UI: `http://<cpa-host>:<port>/v0/resource/plugins/cpa-suite/ui`.

## Not compatible with cpa-key-billing

CPA Suite contains all of cpa-key-billing's features; running both would double-charge and fight over credential selection. If cpa-key-billing is present in `plugins/`, CPA Suite stays in watch-only mode and shows the conflict. Remove cpa-key-billing first.

## Migrating from cpa-key-billing

1. Stop CPA and back up `plugins/cpa-key-billing-state-v1.db`.
2. Move the cpa-key-billing library out of `plugins/`.
3. Install CPA Suite and set `import_from` under `plugins.configs.cpa-suite` to the old database.
4. Start CPA. On first start the data is copied read-only into `plugins/cpa-suite.db`; the old database is never modified.
5. To roll back, remove the cpa-suite library and restore cpa-key-billing.

## Build

`scripts/build.sh` (Go + zig) builds linux/amd64, linux/arm64 and windows/amd64; pushing a `v*` tag runs `.github/workflows/release.yml`, which builds all five platforms and publishes the zips and `checksums.txt`.

## License

MIT, see [LICENSE](LICENSE).
