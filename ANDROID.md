# Android Integration

## Prerequisites

- Android NDK (set `ANDROID_NDK_HOME`)
- Rust with Android targets (`rustup target add aarch64-linux-android`)
- Go 1.22+ with CGO cross-compilation support

## Building

```bash
# Build for arm64 (most devices)
make android

# Build for x86_64 (emulators)
make build-android-x86_64

# Rust libraries only (all architectures)
make build-rust-android
```

The `ANDROID_API` level defaults to 21 (Android 5.0) and can be overridden:

```bash
ANDROID_API=26 make android
```

## Integration Architecture

The Go node runs as a background service in the Android app. The recommended
integration path is to bundle the binary and communicate with it via the
built-in API server (`--api 127.0.0.1:8332`).

### Option A: Bundled Binary (simplest)

1. Place the `blocknet-android-arm64` binary in `app/src/main/jniLibs/arm64-v8a/`
2. Start it as a subprocess from your Foreground Service
3. Communicate via `http://127.0.0.1:8332/api/...`

### Option B: Shared Library (c-shared)

Build as a shared library with exported C functions for JNI:

```bash
make build-android-lib
# produces releases/libblocknet.so + releases/libblocknet.h
```

Place `libblocknet.so` in `app/src/main/jniLibs/arm64-v8a/`. The library
exports the following functions:

| Export | Signature | Description |
|--------|-----------|-------------|
| `BlocknetStart` | `(dataDir, configDir, apiAddr *char) int` | Start node in daemon mode with API. Returns 0=ok, -1=already running, -2=error |
| `BlocknetStop` | `() int` | Graceful shutdown (blocks until stopped). Returns 0=ok, -1=not running |
| `BlocknetIsRunning` | `() int` | Returns 1 if running, 0 otherwise |
| `BlocknetVersion` | `() *char` | Returns version string (caller must free) |
| `BlocknetLastError` | `() *char` | Returns last error string or NULL (caller must free) |
| `BlocknetFree` | `(ptr *void)` | Free a string returned by other exports |

Once started, use the REST API at `http://<apiAddr>/api/...` for all
wallet and node operations (send, balance, status, wallet load, etc.).

## Foreground Service Requirements

Android kills background processes aggressively. The node **must** run inside
a Foreground Service with a persistent notification. Without this, the OS will
terminate the process within minutes of the app leaving the foreground.

### Required Android Manifest Entries

```xml
<uses-permission android:name="android.permission.FOREGROUND_SERVICE" />
<uses-permission android:name="android.permission.FOREGROUND_SERVICE_DATA_SYNC" />
<uses-permission android:name="android.permission.POST_NOTIFICATIONS" />

<service
    android:name=".NodeService"
    android:foregroundServiceType="dataSync"
    android:exported="false" />
```

### Battery Optimization

Request the user to exempt the app from battery optimization (Doze mode).
Without this exemption, the OS will throttle network access and defer
wakeups when the device is idle.

```kotlin
val pm = getSystemService(PowerManager::class.java)
if (!pm.isIgnoringBatteryOptimizations(packageName)) {
    startActivity(Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS).apply {
        data = Uri.parse("package:$packageName")
    })
}
```

### Service Lifecycle

```kotlin
class NodeService : Service() {
    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val notification = buildPersistentNotification()
        startForeground(NOTIFICATION_ID, notification)

        // Start the node with app-specific directories
        val dataDir = filesDir.resolve("blocknet-data").absolutePath
        val configDir = filesDir.absolutePath
        // ... start node process or call native library ...

        return START_STICKY
    }

    override fun onDestroy() {
        // Trigger graceful shutdown (CLI.Shutdown() or send SIGTERM)
        super.onDestroy()
    }
}
```

## Platform Differences

| Feature | Desktop | Android |
|---------|---------|---------|
| Mining | Supported | Disabled (2GB RAM per hash) |
| UPnP/NAT-PMP | Enabled | Disabled (carrier NAT) |
| Relay client | Disabled | Enabled (NAT traversal) |
| Hole punching | Enabled | Enabled |
| Config dir | `os.UserConfigDir()` | Passed from host app via `SetConfigDir()` |
| Shutdown | Unix signals | `CLI.Shutdown()` or context cancellation |
| Data dir | `./blocknet-data-mainnet` | App-specific `filesDir` |

## Storage

Pass the Android app's `filesDir` as both `--data` and `ConfigDir` when
starting the node. Do not use external storage — it may be removable and
is not protected by the app sandbox.

## Network Considerations

- Mobile networks are heavily NATed; the node relies on relay peers and
  hole punching for inbound connectivity.
- Wi-Fi networks may support direct connections but UPnP is still unreliable.
- The libp2p relay client is enabled automatically on Android builds.
- Bandwidth usage should be monitored — consider limiting peer counts
  via `--p2p-max-inbound` and `--p2p-max-outbound` on metered connections.
