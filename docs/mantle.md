# Mantle

Mantle is a supervisory program that wraps the Blocknet daemon ("core") and manages its lifecycle, upgrades, and configuration. The name comes from geology — the mantle is the layer that surrounds the core.

The current Blocknet binary does several things: interactive CLI, HTTP API, daemon mode, wallet, and upgrade notifications. It is commonly embedded as a single binary into other applications (GUI wallets, blockchain visualizers, games, etc.). Users frequently want a set-it-and-forget-it deployment but are pestered with upgrade notices and must manually stop, replace, and restart the binary.

Mantle solves this by splitting the binary into two parts:

- **blocknet-core**: the current daemon stripped of its interactive CLI. Retains the API, wallet, daemon mode, and all chain/mempool/mining/p2p functionality. Always runs in headless daemon mode.
- **mantle**: a supervisory program that spawns and manages one or more core instances as child processes. Owns the CLI, handles upgrades, manages configuration, and communicates with cores via their existing HTTP API.

Mantle never needs to stop for a core upgrade. It downloads the new core binary, gracefully stops the old core, swaps the binary, and restarts — all while mantle itself (and any other running cores) remain up.

## Process Architecture

- Mantle spawns cores as **child processes**, not in-process. Go plugin hot-swapping is fragile and not suitable here.
- Mantle communicates with each core via its **existing HTTP API** and cookie-based auth. No new IPC protocol needed.
- Mantle manages the PID of each core, captures stdout/stderr, and performs health checks via the API.
- Each core instance is fully isolated: separate data directory, ports, wallet, and chain database.
- Mantle propagates signals — when mantle receives SIGTERM/SIGINT, it gracefully stops all managed cores before exiting.

## Core Lifecycle Management

- **Start, stop, restart** individual cores by network (mainnet, testnet).
- **Enable/disable** cores. Enabled cores auto-start when mantle starts. Disabled cores are not started but retain their configuration.
- **Automatic restart on crash** with exponential backoff to avoid restart loops.
- **Simultaneous mainnet and testnet** — each core runs in its own process with isolated config, data, ports, and wallet.

## Upgrade Management

- Periodic check against GitHub releases, reusing the existing version check mechanism (`https://api.github.com/repos/blocknetprivacy/blocknet/releases/latest`).
- **Auto-download and verify** new core binaries.
- **Hot-swap upgrade**: stop a core, replace its binary, restart it. Mantle never stops. Other running cores (e.g. testnet while upgrading mainnet) keep running uninterrupted.
- **Rollback**: if a new core fails to start or crashes shortly after upgrade, automatically revert to the previous version.
- **Version pinning**: opt a specific core out of auto-upgrade by pinning it to a version (`mantle pin mainnet 1.4.2`).
- **Retain previous versions**: keep N previous core binaries on disk for rollback purposes.
- **Notify vs auto-upgrade**: per-core setting. In notify mode, mantle logs that an upgrade is available but does not apply it. In auto-upgrade mode, mantle downloads and applies it automatically.

## Configuration

A single config file for mantle itself and per-core settings. The current daemon uses only flags and environment variables — mantle replaces this with a persistent config file so settings are configured once and remembered.

Sensible defaults allow a zero-config start: mainnet on default ports, testnet on offset ports.

### Example Config

```toml
[mantle]
auto_upgrade = true
check_interval = "24h"
core_binary = "blocknet-core"

[cores.mainnet]
enabled = true
api = "127.0.0.1:8332"
listen = "0.0.0.0:8333"
wallet = true
auto_upgrade = true

[cores.testnet]
enabled = false
api = "127.0.0.1:18332"
listen = "0.0.0.0:18333"
wallet = true
auto_upgrade = true
pinned_version = ""
```

## CLI

The interactive CLI moves from the core to mantle. All current wallet and daemon commands are retained, but they are routed to the target core via its HTTP API instead of being called in-process.

### Core Management Commands

| Command | Description |
|---|---|
| `mantle start [mainnet\|testnet]` | Start a core |
| `mantle stop [mainnet\|testnet]` | Stop a core |
| `mantle restart [mainnet\|testnet]` | Restart a core |
| `mantle enable [mainnet\|testnet]` | Enable auto-start for a core |
| `mantle disable [mainnet\|testnet]` | Disable auto-start for a core |
| `mantle status` | Show status of all managed cores |
| `mantle upgrade [mainnet\|testnet]` | Manually trigger a core upgrade |
| `mantle rollback [mainnet\|testnet]` | Revert a core to its previous version |
| `mantle pin [network] [version]` | Pin a core to a specific version |
| `mantle unpin [network]` | Remove a version pin |
| `mantle attach [mainnet\|testnet]` | Enter an interactive CLI session against a running core |
| `mantle config` | Show or edit configuration |
| `mantle versions` | List downloaded core versions |

### Interactive Mode

`mantle attach mainnet` drops into the same interactive prompt that exists today (`> `), with all the same commands (balance, send, address, history, status, peers, mining, etc.). The difference is that commands are sent to the core over its API rather than called in-process.

Default target is mainnet if not specified and only one core is running.

### Existing Commands (moved from core)

These commands are available inside `mantle attach`:

| Category | Commands |
|---|---|
| Wallet | `help`, `balance`, `address`, `send`, `sign`, `verify`, `history`, `outputs`, `seed`, `import`, `viewkeys`, `lock`, `unlock`, `save`, `sync` |
| Daemon | `status`, `peers`, `banned`, `export-peer`, `mining`, `certify`, `purge` |
| Meta | `version`, `about`, `license`, `quit` |

## What Changes in the Core

The core (`blocknet-core`) is the current `blocknet` binary with the following removed:

- Interactive CLI and prompt loop
- Periodic version check and upgrade notification (mantle handles this)
- The `--daemon` flag becomes unnecessary since the core always runs headless

Everything else stays: the HTTP API, wallet, chain, mempool, miner, p2p, sync manager, stealth keys, SSE events, mining API, and all existing API routes.

## Distribution

Mantle is the primary distribution artifact. Users download mantle, which ships with a bundled core binary. Users never need to think about "core" as a separate thing — mantle is the product, core is an internal implementation detail.

Mantle can then download newer core versions for upgrades, but the initial download is fully self-contained and works offline.

## Target Platforms

Mantle targets Windows, macOS (Apple Silicon), Linux, and Android.

### Platform-Specific Init Integration

Each platform has its own mechanism for running mantle as a background service that starts on boot and restarts on failure:

- **Linux**: systemd service file (`mantle.service`)
- **macOS**: launchd plist (`~/Library/LaunchAgents/com.blocknet.mantle.plist`)
- **Windows**: Windows Service (via `golang.org/x/sys/windows/svc`) or Task Scheduler

### Android

Android is a special case. On Android, the daemon is always embedded into a host app (wallet, game, etc.), and the app itself manages the process lifecycle. Mantle as a standalone binary doesn't fit Android's model — there's no terminal, no init system, and users don't launch binaries directly. On Android, the host app manages core directly without mantle, or mantle is embedded as a library rather than a standalone supervisor.

## Self-Update

Mantle supports self-update, but keeps it simple. Mantle is a thin supervisor — it changes rarely compared to the core (which has consensus rules, protocol changes, wallet features, etc.). The worst case of running an old mantle is missing a new mantle feature, not forking off the network.

Self-update mechanism: mantle downloads the new mantle binary, replaces itself on disk, and picks up the new version on next restart. If running behind a system service (systemd, launchd, Windows Service), mantle triggers a service restart after replacing the binary. Cores are gracefully stopped on shutdown and auto-started by the new mantle on startup. Not a priority for the MVP.

## Decisions

- **Remote `mantle attach`** is not in scope for the MVP. `attach` connects to local cores only. Remote management can be added later since the HTTP API transport makes it trivial — it's just a matter of pointing at a different address.
- **Binary distribution**: mantle ships bundled with a core binary. Single download, works immediately, no first-run network dependency.
- **Init integration**: ship platform-specific service files for Linux, macOS, and Windows. Android apps manage core directly.
