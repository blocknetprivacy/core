//go:build android

package main

/*
#include <stdlib.h>
*/
import "C"
import (
	"sync"
	"unsafe"

	"blocknet/p2p"
	"blocknet/wallet"
)

var (
	mobileMu   sync.Mutex
	activeCLI  *CLI
	activeErr  error
	runningWg  sync.WaitGroup
)

// BlocknetVersion returns the version string. Caller must C.free the result.
//
//export BlocknetVersion
func BlocknetVersion() *C.char {
	return C.CString(Version)
}

// BlocknetStart starts the node in daemon mode with the API server.
// dataDir: path for chain data (e.g. Android filesDir + "/blocknet-data")
// configDir: path for config/identity/backups (e.g. Android filesDir)
// apiAddr: API listen address (e.g. "127.0.0.1:8332")
// Returns 0 on success, -1 if already running, -2 on startup error.
//
//export BlocknetStart
func BlocknetStart(dataDir, configDir, apiAddr *C.char) C.int {
	mobileMu.Lock()
	defer mobileMu.Unlock()

	if activeCLI != nil {
		return -1
	}

	goDataDir := C.GoString(dataDir)
	goConfigDir := C.GoString(configDir)
	goAPIAddr := C.GoString(apiAddr)

	if goConfigDir != "" {
		mainConfigDir = goConfigDir
		wallet.SetConfigDir(goConfigDir)
		p2p.SetConfigDir(goConfigDir)
	}

	seedNodes := ResolveSeedNodes(DefaultSeedHosts, MainnetP2PPort, MainnetPeerIDPort)
	if len(seedNodes) == 0 {
		seedNodes = DefaultSeedNodes
	}

	cfg := CLIConfig{
		DataDir:    goDataDir,
		ConfigDir:  goConfigDir,
		WalletFile: DefaultWalletFilename,
		ListenAddrs: []string{"/ip4/0.0.0.0/tcp/28080"},
		SeedNodes:  seedNodes,
		DaemonMode: true,
		APIAddr:    goAPIAddr,
		NoColor:    true,
	}

	cli, err := NewCLI(cfg)
	if err != nil {
		activeErr = err
		return -2
	}

	activeCLI = cli
	activeErr = nil

	runningWg.Add(1)
	go func() {
		defer runningWg.Done()
		if err := cli.Run(); err != nil {
			mobileMu.Lock()
			activeErr = err
			mobileMu.Unlock()
		}
		mobileMu.Lock()
		activeCLI = nil
		mobileMu.Unlock()
	}()

	return 0
}

// BlocknetStop triggers a graceful shutdown. Blocks until the node has stopped.
// Returns 0 on success, -1 if not running.
//
//export BlocknetStop
func BlocknetStop() C.int {
	mobileMu.Lock()
	cli := activeCLI
	mobileMu.Unlock()

	if cli == nil {
		return -1
	}

	_ = cli.Shutdown()
	runningWg.Wait()
	return 0
}

// BlocknetIsRunning returns 1 if the node is running, 0 otherwise.
//
//export BlocknetIsRunning
func BlocknetIsRunning() C.int {
	mobileMu.Lock()
	defer mobileMu.Unlock()
	if activeCLI != nil {
		return 1
	}
	return 0
}

// BlocknetLastError returns the last startup/runtime error, or NULL if none.
// Caller must C.free the result.
//
//export BlocknetLastError
func BlocknetLastError() *C.char {
	mobileMu.Lock()
	defer mobileMu.Unlock()
	if activeErr == nil {
		return nil
	}
	return C.CString(activeErr.Error())
}

// Free is a convenience for Android/JNI callers to free C strings
// returned by other exports without needing to link against libc directly.
//
//export BlocknetFree
func BlocknetFree(ptr unsafe.Pointer) {
	C.free(ptr)
}
