package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"blocknet/wallet"
	"golang.org/x/time/rate"
)

const maxRequestBodyBytes int64 = 1 << 20 // 1MB

const (
	unlockFailureBaseDelay  = 250 * time.Millisecond
	unlockFailureMaxDelay   = 5 * time.Second
	unlockFailureLockout    = 30 * time.Second
	unlockFailureStateTTL   = 30 * time.Minute
	unlockFailuresToLockout = 8
)

// APIServer serves the authenticated JSON API for GUI wallets.
type APIServer struct {
	daemon  *Daemon
	wallet  *wallet.Wallet
	scanner *wallet.Scanner
	dataDir string
	token   string
	server  *http.Server

	// Wallet lock state (mirrors CLI behavior)
	locked          bool
	walletLoading   bool
	passwordHash    [32]byte
	passwordHashSet bool
	mu              sync.RWMutex

	// Back-reference to CLI for wallet hot-loading in daemon mode
	cli *CLI

	// Route-scoped abuse controls for expensive block validation.
	submitBlockLimiter *perIPLimiter
	submitBlockSem     chan struct{}

	// Route-scoped abuse controls for expensive tx construction/signing.
	sendLimiter *perIPLimiter
	sendSem     chan struct{}
	sendIdem    *idempotencyCache

	// Route-scoped abuse controls for stateless signature verification.
	verifyLimiter *perIPLimiter

	// Wallet unlock brute-force controls.
	unlockAttempts *unlockAttemptTracker

	// Template cache for compact mining submissions ({template_id, nonce}).
	templateMu    sync.Mutex
	templateCache map[string]cachedMiningTemplate
	templateNow   func() time.Time
	templateTTL   time.Duration
}

type cachedMiningTemplate struct {
	block     Block
	expiresAt time.Time
}

const (
	// Never exceed this bound or evict an unexpired same-tip lease. New
	// template requests fail until a lease expires or the chain tip changes.
	maxMiningTemplateCacheEntries = 512
	miningTemplateCacheTTL        = 10 * time.Minute
)

type perIPLimiter struct {
	mu      sync.Mutex
	clients map[string]*ipClientLimiter
	limit   rate.Limit
	burst   int
	ttl     time.Duration
}

type ipClientLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type unlockAttemptTracker struct {
	mu      sync.Mutex
	clients map[string]*unlockAttemptState
}

type unlockAttemptState struct {
	failures     int
	nextAllowed  time.Time
	lockoutUntil time.Time
	lastSeen     time.Time
}

func newPerIPLimiter(limit rate.Limit, burst int, ttl time.Duration) *perIPLimiter {
	return &perIPLimiter{
		clients: make(map[string]*ipClientLimiter),
		limit:   limit,
		burst:   burst,
		ttl:     ttl,
	}
}

func newUnlockAttemptTracker() *unlockAttemptTracker {
	return &unlockAttemptTracker{
		clients: make(map[string]*unlockAttemptState),
	}
}

func (t *unlockAttemptTracker) precheck(ip string) (time.Duration, time.Time) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	for key, state := range t.clients {
		if now.Sub(state.lastSeen) > unlockFailureStateTTL {
			delete(t.clients, key)
		}
	}

	state, ok := t.clients[ip]
	if !ok {
		return 0, time.Time{}
	}
	state.lastSeen = now

	if now.Before(state.lockoutUntil) {
		return 0, state.lockoutUntil
	}
	if now.Before(state.nextAllowed) {
		return state.nextAllowed.Sub(now), time.Time{}
	}
	return 0, time.Time{}
}

func (t *unlockAttemptTracker) recordFailure(ip string) (time.Duration, time.Time) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	state, ok := t.clients[ip]
	if !ok {
		state = &unlockAttemptState{}
		t.clients[ip] = state
	}

	state.lastSeen = now
	state.failures++

	shift := state.failures - 1
	if shift > 5 {
		shift = 5
	}
	delay := unlockFailureBaseDelay << shift
	if delay > unlockFailureMaxDelay {
		delay = unlockFailureMaxDelay
	}
	state.nextAllowed = now.Add(delay)

	if state.failures >= unlockFailuresToLockout {
		state.lockoutUntil = now.Add(unlockFailureLockout)
		state.nextAllowed = state.lockoutUntil
		state.failures = 0
		return delay, state.lockoutUntil
	}

	return delay, time.Time{}
}

func (t *unlockAttemptTracker) recordSuccess(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.clients, ip)
}

func (l *perIPLimiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// Opportunistic cleanup keeps map bounded without extra goroutines.
	for key, client := range l.clients {
		if now.Sub(client.lastSeen) > l.ttl {
			delete(l.clients, key)
		}
	}

	client, ok := l.clients[ip]
	if !ok {
		client = &ipClientLimiter{
			limiter: rate.NewLimiter(l.limit, l.burst),
		}
		l.clients[ip] = client
	}
	client.lastSeen = now
	return client.limiter.Allow()
}

// NewAPIServer creates a new API server. wallet and scanner may be nil
// for public-only mode (e.g. seed node running --explorer).
func NewAPIServer(daemon *Daemon, w *wallet.Wallet, scanner *wallet.Scanner, dataDir string, password []byte) *APIServer {
	s := &APIServer{
		daemon:             daemon,
		wallet:             w,
		scanner:            scanner,
		dataDir:            dataDir,
		submitBlockLimiter: newPerIPLimiter(rate.Limit(2), 4, 10*time.Minute),
		submitBlockSem:     make(chan struct{}, 2),
		sendLimiter:        newPerIPLimiter(rate.Limit(0.5), 2, 10*time.Minute), // ~1 req / 2s, burst 2
		sendSem:            make(chan struct{}, 1),
		sendIdem:           newIdempotencyCache(30*24*time.Hour, 4096, filepath.Join(dataDir, "send-idempotency.json")),
		verifyLimiter:      newPerIPLimiter(rate.Limit(5), 10, 10*time.Minute),
		unlockAttempts:     newUnlockAttemptTracker(),
		templateCache:      make(map[string]cachedMiningTemplate),
	}
	if len(password) > 0 {
		s.passwordHash = passwordHash(password)
		s.passwordHashSet = true
	}
	return s
}

func (s *APIServer) rememberMiningTemplateLease(block *Block) (string, time.Time, error) {
	if block == nil {
		return "", time.Time{}, errors.New("cannot cache nil mining template")
	}

	s.templateMu.Lock()
	defer s.templateMu.Unlock()
	now := s.miningTemplateNow()
	expiresAt := now.Add(s.miningTemplateTTL())

	if s.daemon == nil || s.daemon.Chain() == nil {
		return "", time.Time{}, errors.New("cannot cache mining template without chain state")
	}
	// Snapshot canonical state only after winning the cache lock. An old request
	// may have built its block before a tip change and then waited behind a newer
	// request; reject it before it can prune the newer tip's advertised leases.
	canonical := s.daemon.Chain().TemplateParams()
	if block.Header.Height != canonical.Height || block.Header.PrevHash != canonical.PrevHash {
		return "", time.Time{}, errMiningTemplateStale
	}

	s.pruneMiningTemplatesForTipLocked(now, canonical)
	if s.templateCache == nil {
		s.templateCache = make(map[string]cachedMiningTemplate)
	}
	if len(s.templateCache) >= maxMiningTemplateCacheEntries {
		return "", time.Time{}, &miningTemplateCacheFullError{
			retryAfterSeconds: earliestMiningTemplateRetryAfterSeconds(s.templateCache, now),
		}
	}

	var templateID string
	for attempt := int64(0); ; attempt++ {
		idBytes := make([]byte, 8)
		if _, err := rand.Read(idBytes); err != nil {
			fallback := now.UnixNano() + attempt
			for i := 0; i < len(idBytes); i++ {
				idBytes[i] = byte(fallback >> (8 * i))
			}
		}
		candidate := hex.EncodeToString(idBytes)
		if _, exists := s.templateCache[candidate]; !exists {
			templateID = candidate
			break
		}
	}

	s.templateCache[templateID] = cachedMiningTemplate{
		block:     *block,
		expiresAt: expiresAt,
	}

	return templateID, expiresAt, nil
}

func (s *APIServer) getMiningTemplate(templateID string) (*Block, bool) {
	if templateID == "" {
		return nil, false
	}

	s.templateMu.Lock()
	defer s.templateMu.Unlock()
	now := s.miningTemplateNow()

	s.pruneMiningTemplatesLocked(now)
	tpl, ok := s.templateCache[templateID]
	if !ok {
		return nil, false
	}
	block := tpl.block
	return &block, true
}

var (
	errMiningTemplateCacheFull   = errors.New("mining template cache is full")
	errMiningTemplateUnavailable = errors.New("mining template is unknown or expired")
	errMiningTemplateStale       = errors.New("mining template does not build on the current tip")
)

type miningTemplateCacheFullError struct {
	retryAfterSeconds int64
}

func (e *miningTemplateCacheFullError) Error() string {
	return errMiningTemplateCacheFull.Error()
}

func (e *miningTemplateCacheFullError) Unwrap() error {
	return errMiningTemplateCacheFull
}

func earliestMiningTemplateRetryAfterSeconds(cache map[string]cachedMiningTemplate, now time.Time) int64 {
	var earliest time.Time
	for _, tpl := range cache {
		if earliest.IsZero() || tpl.expiresAt.Before(earliest) {
			earliest = tpl.expiresAt
		}
	}
	if earliest.IsZero() || !earliest.After(now) {
		return 1
	}
	remaining := earliest.Sub(now)
	seconds := int64((remaining + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func (s *APIServer) renewMiningTemplate(templateID string) (time.Time, error) {
	if templateID == "" {
		return time.Time{}, errMiningTemplateUnavailable
	}

	s.templateMu.Lock()
	defer s.templateMu.Unlock()
	now := s.miningTemplateNow()

	s.pruneMiningTemplatesLocked(now)
	tpl, ok := s.templateCache[templateID]
	if !ok {
		return time.Time{}, errMiningTemplateUnavailable
	}

	if s.daemon == nil || s.daemon.Chain() == nil {
		return time.Time{}, errors.New("cannot renew mining template without chain state")
	}
	tip := s.daemon.Chain().TemplateParams()
	if tpl.block.Header.Height != tip.Height || tpl.block.Header.PrevHash != tip.PrevHash {
		return time.Time{}, errMiningTemplateStale
	}

	tpl.expiresAt = now.Add(s.miningTemplateTTL())
	s.templateCache[templateID] = tpl
	return tpl.expiresAt, nil
}

func (s *APIServer) miningTemplateNow() time.Time {
	var now time.Time
	if s.templateNow != nil {
		now = s.templateNow()
	} else {
		now = time.Now()
	}

	// Lease deadlines are exposed as Unix milliseconds. Normalize both the
	// production clock and injected test clocks to the same wall-clock basis so
	// pruning happens exactly at the advertised instant, without Go's hidden
	// monotonic component affecting comparisons.
	return time.UnixMilli(now.UnixMilli()).In(now.Location())
}

func (s *APIServer) miningTemplateTTL() time.Duration {
	if s.templateTTL > 0 {
		return s.templateTTL
	}
	return miningTemplateCacheTTL
}

func (s *APIServer) pruneMiningTemplatesLocked(now time.Time) {
	for id, tpl := range s.templateCache {
		if !now.Before(tpl.expiresAt) {
			delete(s.templateCache, id)
		}
	}
}

func (s *APIServer) pruneMiningTemplatesForTipLocked(now time.Time, tip BlockTemplateParams) {
	for id, tpl := range s.templateCache {
		if !now.Before(tpl.expiresAt) ||
			tpl.block.Header.Height != tip.Height ||
			tpl.block.Header.PrevHash != tip.PrevHash {
			delete(s.templateCache, id)
		}
	}
}

// isLocked returns whether the wallet is locked.
func (s *APIServer) isLocked() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.locked
}

// requireWallet checks that the wallet exists and is unlocked.
// Returns true if the request should proceed.
func (s *APIServer) requireWallet(w http.ResponseWriter, _ *http.Request) bool {
	if s.wallet == nil {
		writeError(w, http.StatusServiceUnavailable, "no wallet loaded")
		return false
	}
	if s.isLocked() {
		writeError(w, http.StatusForbidden, "wallet is locked")
		return false
	}
	return true
}

// Start launches the full authenticated API server.
func (s *APIServer) Start(addr string) error {

	token, err := generateToken()
	if err != nil {
		return fmt.Errorf("failed to generate auth token: %w", err)
	}
	s.token = token

	if err := writeCookie(s.dataDir, token); err != nil {
		deleteCookie(s.dataDir)
		return fmt.Errorf("failed to write cookie: %w", err)
	}

	privateMux := http.NewServeMux()
	s.registerPrivateRoutes(privateMux)

	topMux := http.NewServeMux()
	s.registerPublicRoutes(topMux)
	topMux.Handle("/", authMiddleware(token, privateMux))

	var handler http.Handler = topMux
	handler = maxBodySize(handler, maxRequestBodyBytes)

	s.server = &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		deleteCookie(s.dataDir)
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("API server error: %v", err)
		}
	}()

	return nil
}

func isInsecureAPIBindAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return !ip.IsLoopback()
}

// Stop gracefully shuts down the API server and removes the cookie file.
func (s *APIServer) Stop() {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(ctx); err != nil {
			log.Printf("Warning: API shutdown error: %v", err)
		}
	}
	deleteCookie(s.dataDir)
}

// maxBodySize limits request body size to prevent OOM from large payloads.
func maxBodySize(next http.Handler, bytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, bytes)
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}
