package main

import "net/http"

// registerPublicRoutes adds stateless endpoints that don't require a wallet.
// These are shared between --api (authenticated) and --explorer (public).
func (s *APIServer) registerPublicRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/block/{id}", s.handleBlock)
	mux.HandleFunc("GET /api/tx/{hash}", s.handleTx)
	mux.HandleFunc("GET /api/mempool", s.handleMempool)
	mux.HandleFunc("GET /api/mempool/txs", s.handleMempoolTxs)
	mux.HandleFunc("GET /api/peers", s.handlePeers)
	mux.HandleFunc("GET /api/peers/banned", s.handleBannedPeers)
	mux.HandleFunc("POST /api/verify", s.handleVerify)
}

// registerPrivateRoutes adds wallet, mining, and control endpoints.
// Only registered for --api (always behind auth).
func (s *APIServer) registerPrivateRoutes(mux *http.ServeMux) {
	// Wallet
	mux.HandleFunc("POST /api/wallet/load", s.handleLoadWallet)
	mux.HandleFunc("POST /api/wallet/import", s.handleImportWallet)
	mux.HandleFunc("GET /api/wallet/balance", s.handleBalance)
	mux.HandleFunc("GET /api/wallet/address", s.handleAddress)
	mux.HandleFunc("GET /api/wallet/history", s.handleHistory)
	mux.HandleFunc("GET /api/wallet/outputs", s.handleOutputs)
	mux.HandleFunc("POST /api/wallet/send", s.handleSend)
	mux.HandleFunc("POST /api/wallet/send/advanced", s.handleSendAdvanced)
	mux.HandleFunc("POST /api/wallet/sign", s.handleSign)
	mux.HandleFunc("POST /api/wallet/lock", s.handleLock)
	mux.HandleFunc("POST /api/wallet/unlock", s.handleUnlock)
	mux.HandleFunc("POST /api/wallet/seed", s.handleSeed)
	mux.HandleFunc("POST /api/wallet/sync", s.handleWalletSync)
	mux.HandleFunc("POST /api/wallet/prove", s.handleProve)
	mux.HandleFunc("POST /api/wallet/audit", s.handleAudit)
	mux.HandleFunc("POST /api/wallet/viewkeys", s.handleViewKeys)

	// Chain verification
	mux.HandleFunc("GET /api/certify", s.handleCertify)

	// Mining
	mux.HandleFunc("GET /api/mining", s.handleMiningStatus)
	mux.HandleFunc("POST /api/mining/start", s.handleMiningStart)
	mux.HandleFunc("POST /api/mining/stop", s.handleMiningStop)
	mux.HandleFunc("POST /api/mining/threads", s.handleMiningThreads)
	mux.HandleFunc("GET /api/mining/blocktemplate", s.handleBlockTemplate)
	mux.HandleFunc("POST /api/mining/submitblock", s.handleSubmitBlock)

	// Dangerous operations
	mux.HandleFunc("POST /api/purge", s.handlePurgeData)

	// SSE
	mux.HandleFunc("GET /api/events", s.handleEvents)
}
