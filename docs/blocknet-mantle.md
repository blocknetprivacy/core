# Mantle

Mantle is a supervisory program that wraps the Blocknet daemon ("core") and manages its lifecycle, upgrades, and configuration. The name comes from geology — the mantle is the layer that surrounds the core.

The current Blocknet binary does several things: interactive CLI, HTTP API, daemon mode, wallet, and upgrade notifications. It is commonly embedded as a single binary into other applications (GUI wallets, blockchain visualizers, games, etc.). Users frequently want a set-it-and-forget-it deployment but are pestered with upgrade notices and must manually stop, replace, and restart the binary.

Mantle solves this by splitting the binary into two parts:

- **core**: the current daemon stripped of its interactive CLI. Retains the API, wallet, daemon mode, and all chain/mempool/mining/p2p functionality. Always runs in headless daemon mode. Lives at `github.com/blocknetprivacy/core`.
- **blocknet** (mantle): a supervisory program that spawns and manages one or more core instances as child processes. Owns the CLI, handles upgrades, manages configuration, and communicates with cores via their existing HTTP API. Lives at `github.com/blocknetprivacy/blocknet` (takes over the current repo name).

The binary is called `blocknet` (with `bnt` as a short alias). Users interact with `blocknet` or `bnt` — "mantle" is the internal project name only.

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

## Version Management

Mantle manages core versions with a package-manager-like interface. Cores are stored in `~/.config/mantle/cores/{version}/` and can be installed, removed, and switched between without affecting running instances until explicitly applied.

### Nightly Builds

The `nightly` version always tracks the latest commit on the master branch of `blocknetprivacy/core`. A CI workflow builds binaries on every push to master and publishes them under a rolling `nightly` release tag. Mantle can install and use nightly like any other version.

### Version Resolution

`blocknet use` sets which core version to run. It works at two levels:

- **Global default**: `blocknet use v0.7.0` sets v0.7.0 as the default for all cores.
- **Per-network override**: `blocknet use v0.6.9 testnet` overrides the global default for testnet only.

A core set to a specific version via `use` is implicitly pinned — `blocknet upgrade` will not change it. To return a core to tracking the latest release, use `blocknet use latest` or `blocknet use latest mainnet`.

### Upgrade Flow

1. `blocknet update` — checks `github.com/blocknetprivacy/core/releases` for new tagged releases. Reports what's available.
2. `blocknet upgrade` — if a new release is available and the core is not pinned, downloads it, stops the core, swaps the binary, restarts. Other running cores are unaffected.

Auto-upgrade can be enabled in config so this happens without manual intervention.

## Configuration

A single JSON config file at `~/.config/mantle/config.json`. The current daemon uses only flags and environment variables — mantle replaces this with a persistent config file so settings are configured once and remembered.

The config parser strips `//` and `#` line comments before parsing, so the file can be annotated without breaking standard JSON parsing. Parsed with the stdlib `encoding/json` — no third-party config libraries.

Sensible defaults allow a zero-config start: mainnet on default ports, testnet on offset ports.

### Example Config

```json
{
  // mantle-level settings
  "auto_upgrade": true,
  "check_interval": "24h",

  "cores": {
    "mainnet": {
      "enabled": true,
      "api": "127.0.0.1:8332",
      "listen": "0.0.0.0:8333",
      "wallet": true,
      "version": "latest"
    },
    "testnet": {
      "enabled": false,
      "api": "127.0.0.1:18332",
      "listen": "0.0.0.0:18333",
      "wallet": true,
      "version": "latest"
    }
  }
}
```

## File Layout

All mantle data lives under XDG config:

```
~/.config/mantle/
├── cores/
│   ├── v0.7.0/
│   │   └── core          # core binary for this version
│   ├── v0.6.9/
│   │   └── core
│   └── nightly/
│       └── core
├── config.json
├── mantle.log
├── mantle.mainnet.log
└── mantle.testnet.log
```

## CLI

The interactive CLI moves from the core to mantle. All current wallet and daemon commands are retained, but they are routed to the target core via its HTTP API instead of being called in-process.

The binary responds to both `blocknet` and `bnt`.

### Commands

| Command | Description |
|---|---|
| `blocknet start [mainnet\|testnet]` | Start a core |
| `blocknet stop [mainnet\|testnet]` | Stop a core |
| `blocknet restart [mainnet\|testnet]` | Restart a core |
| `blocknet enable [mainnet\|testnet]` | Enable auto-start for a core |
| `blocknet disable [mainnet\|testnet]` | Disable auto-start for a core |
| `blocknet status` | Show status of all managed cores |
| `blocknet attach [mainnet\|testnet]` | Interactive CLI session against a running core |
| `blocknet update` | Check for new core releases |
| `blocknet upgrade` | Download and apply the latest core release |
| `blocknet list` | List available and installed core versions |
| `blocknet install <version>` | Download a core version to local storage |
| `blocknet uninstall <version>` | Remove a core version from local storage |
| `blocknet use <version> [network]` | Set which core version to use (globally or per-network) |
| `blocknet config` | Show or edit configuration |

### `blocknet list` Output

```
version     date             status
──────────────────────────────────────────────
nightly     latest
v0.7.0      Feb 01, 2026                        ← installed (cyan)
v0.6.9      Jan 23, 2026     [testnet]           ← in use by testnet (#F0A)
v0.6.8      Jan 21, 2026     [mainnet]           ← in use by mainnet (#AF0)
v0.5.12     Jan 13, 2026
v0.5.8      Jan 11, 2026
```

- Versions in use by a network are colored: mainnet (#AF0), testnet (#F0A).
- Installed but unused versions are cyan.
- Uninstalled versions are gray/white.
- Nightly always shows "latest" as its date, representing the most recent commit on master.
- Dates come from GitHub release timestamps.

### Interactive Mode

`blocknet attach mainnet` drops into the same interactive prompt that exists today (`> `), with all the same commands (balance, send, address, history, status, peers, mining, etc.). Commands are sent to the core over its API rather than called in-process.

Default target is mainnet if not specified and only one core is running.

Not in scope for the MVP — local only, no remote attach.

### Existing Commands (moved from core)

These commands are available inside `blocknet attach`:

| Category | Commands |
|---|---|
| Wallet | `help`, `balance`, `address`, `send`, `sign`, `verify`, `history`, `outputs`, `seed`, `import`, `viewkeys`, `lock`, `unlock`, `save`, `sync` |
| Daemon | `status`, `peers`, `banned`, `export-peer`, `mining`, `certify`, `purge` |
| Meta | `version`, `about`, `license`, `quit` |

## What Changes in the Core

The core is the current `blocknet` binary with the following removed:

- Interactive CLI and prompt loop
- Periodic version check and upgrade notification (mantle handles this)
- The `--daemon` flag becomes unnecessary since the core always runs headless

Everything else stays: the HTTP API, wallet, chain, mempool, miner, p2p, sync manager, stealth keys, SSE events, mining API, and all existing API routes.

## Repo Rename

The current repository `github.com/blocknetprivacy/blocknet` is renamed to `github.com/blocknetprivacy/core` to reflect the daemon's new role. The `blocknetprivacy/blocknet` name is then used for the mantle repository, so existing links and bookmarks point to the primary user-facing tool.

| Before | After |
|---|---|
| `blocknetprivacy/blocknet` (daemon + CLI) | `blocknetprivacy/core` (daemon only) |
| — | `blocknetprivacy/blocknet` (mantle) |

## Distribution

Mantle is the primary distribution artifact. Users download `blocknet` (mantle), which ships with a bundled core binary. Users never need to think about "core" as a separate thing — mantle is the product, core is an internal implementation detail.

Mantle can then download newer core versions via `blocknet update && blocknet upgrade`, but the initial download is fully self-contained and works offline.

## Target Platforms

Mantle targets Windows, macOS (Apple Silicon), and Linux.

### Platform-Specific Init Integration

Each platform has its own mechanism for running mantle as a background service that starts on boot and restarts on failure:

- **Linux**: systemd service file (`blocknet.service`)
- **macOS**: launchd plist (`~/Library/LaunchAgents/com.blocknet.plist`)
- **Windows**: Windows Service (via `golang.org/x/sys/windows/svc`) or Task Scheduler

### Android

Android is not a mantle target. On Android, the daemon is embedded into a dedicated app that manages the core directly.

## Self-Update

Mantle supports self-update, but keeps it simple. Mantle is a thin supervisor — it changes rarely compared to the core (which has consensus rules, protocol changes, wallet features, etc.). The worst case of running an old mantle is missing a new mantle feature, not forking off the network.

Self-update mechanism: mantle downloads the new mantle binary, replaces itself on disk, and picks up the new version on next restart. If running behind a system service (systemd, launchd, Windows Service), mantle triggers a service restart after replacing the binary. Cores are gracefully stopped on shutdown and auto-started by the new mantle on startup. Not a priority for the MVP.
