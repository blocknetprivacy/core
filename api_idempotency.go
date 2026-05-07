package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type idempotencyResult struct {
	status int
	body   []byte
}

type idempotencyState string

const (
	idempotencyStateAccepted   idempotencyState = "accepted"
	idempotencyStateInProgress idempotencyState = "in_progress"
	idempotencyStateCompleted  idempotencyState = "completed"
	idempotencyStateFailed     idempotencyState = "failed"
)

type idempotencyCache struct {
	mu          sync.Mutex
	ttl         time.Duration
	maxEntries  int
	entries     map[string]*idempotencyEntry
	persistPath string
}

type idempotencyEntry struct {
	createdAt time.Time
	updatedAt time.Time
	reqHash   [32]byte
	state     idempotencyState
	result    idempotencyResult
}

type persistedIdempotencyEntry struct {
	CreatedAtUnixNano int64  `json:"created_at_unix_nano"`
	UpdatedAtUnixNano int64  `json:"updated_at_unix_nano"`
	ReqHashHex        string `json:"req_hash_hex"`
	State             string `json:"state"`
	Status            int    `json:"status"`
	BodyBase64        string `json:"body_base64"`
}

type persistedIdempotencyState struct {
	Entries map[string]persistedIdempotencyEntry `json:"entries"`
}

func newIdempotencyCache(ttl time.Duration, maxEntries int, persistPath string) *idempotencyCache {
	c := &idempotencyCache{
		ttl:         ttl,
		maxEntries:  maxEntries,
		entries:     make(map[string]*idempotencyEntry),
		persistPath: persistPath,
	}
	c.loadPersisted()
	return c
}

func (c *idempotencyCache) getOrStart(now time.Time, key string, reqHash [32]byte) (state string, res idempotencyResult) {
	// state:
	// - "replay": res is valid
	// - "start": caller should process request and then complete()
	// - "inflight": same key currently running
	// - "mismatch": key exists but request differs
	c.mu.Lock()

	c.pruneLocked(now)

	if e, ok := c.entries[key]; ok {
		if e.reqHash != reqHash {
			c.mu.Unlock()
			return "mismatch", idempotencyResult{}
		}
		if !e.isTerminal() {
			c.mu.Unlock()
			return "inflight", idempotencyResult{}
		}
		c.mu.Unlock()
		return "replay", e.result
	}

	c.entries[key] = &idempotencyEntry{
		createdAt: now,
		updatedAt: now,
		reqHash:   reqHash,
		state:     idempotencyStateInProgress,
	}
	c.enforceCapLocked()
	data, path := c.snapshotForPersist()
	c.mu.Unlock()

	if err := writePersistData(data, path); err != nil {
		log.Printf("Warning: failed to persist idempotency cache: %v", err)
	}
	return "start", idempotencyResult{}
}

func (c *idempotencyCache) complete(now time.Time, key string, reqHash [32]byte, status int, body []byte) {
	c.mu.Lock()

	c.pruneLocked(now)

	e, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		return
	}
	if e.reqHash != reqHash {
		delete(c.entries, key)
		c.mu.Unlock()
		return
	}
	e.updatedAt = now
	if status >= 200 && status < 300 {
		e.state = idempotencyStateCompleted
	} else {
		e.state = idempotencyStateFailed
	}
	e.result = idempotencyResult{status: status, body: append([]byte(nil), body...)}
	c.enforceCapLocked()
	data, path := c.snapshotForPersist()
	c.mu.Unlock()

	if err := writePersistData(data, path); err != nil {
		log.Printf("Warning: failed to persist idempotency cache: %v", err)
	}
}

func (c *idempotencyCache) abandon(key string) {
	c.mu.Lock()
	if _, ok := c.entries[key]; !ok {
		c.mu.Unlock()
		return
	}
	delete(c.entries, key)
	data, path := c.snapshotForPersist()
	c.mu.Unlock()

	if err := writePersistData(data, path); err != nil {
		log.Printf("Warning: failed to persist idempotency cache: %v", err)
	}
}

func (c *idempotencyCache) pruneLocked(now time.Time) {
	if c.ttl <= 0 {
		return
	}
	for k, e := range c.entries {
		// Never expire unresolved entries; dropping them re-allows duplicate processing.
		if !e.isTerminal() {
			continue
		}
		if now.Sub(e.updatedAt) > c.ttl {
			delete(c.entries, k)
		}
	}
}

func (c *idempotencyCache) enforceCapLocked() {
	if c.maxEntries <= 0 || len(c.entries) <= c.maxEntries {
		return
	}

	// Evict oldest completed entries first.
	// Never evict in-flight entries; callers use those to prevent duplicate processing.
	for len(c.entries) > c.maxEntries {
		var oldestKey string
		var oldestTime time.Time
		first := true
		for k, e := range c.entries {
			if !e.isTerminal() {
				continue
			}
			if first || e.updatedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = e.updatedAt
				first = false
			}
		}
		if oldestKey == "" {
			// All remaining entries are unresolved; keep them even if we're over cap.
			return
		}
		delete(c.entries, oldestKey)
	}
}

func hashRequestBody(body []byte) [32]byte {
	return sha256.Sum256(body)
}

type idempotencyLookup struct {
	CreatedAt time.Time
	UpdatedAt time.Time
	State     idempotencyState
	Result    idempotencyResult
}

func (c *idempotencyCache) lookup(now time.Time, key string) (idempotencyLookup, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.pruneLocked(now)

	entry, ok := c.entries[key]
	if !ok {
		return idempotencyLookup{}, false
	}
	return idempotencyLookup{
		CreatedAt: entry.createdAt,
		UpdatedAt: entry.updatedAt,
		State:     entry.state,
		Result: idempotencyResult{
			status: entry.result.status,
			body:   append([]byte(nil), entry.result.body...),
		},
	}, true
}

func (c *idempotencyCache) loadPersisted() {
	if c.persistPath == "" {
		return
	}

	data, err := os.ReadFile(c.persistPath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Printf("Warning: failed to read idempotency cache %s: %v", c.persistPath, err)
		return
	}

	var persisted persistedIdempotencyState
	if err := json.Unmarshal(data, &persisted); err != nil {
		log.Printf("Warning: failed to parse idempotency cache %s: %v", c.persistPath, err)
		return
	}

	now := time.Now()
	for key, entry := range persisted.Entries {
		reqHashBytes, err := hex.DecodeString(entry.ReqHashHex)
		if err != nil || len(reqHashBytes) != sha256.Size {
			continue
		}
		body := []byte(nil)
		if entry.BodyBase64 != "" {
			body, err = base64.StdEncoding.DecodeString(entry.BodyBase64)
			if err != nil {
				continue
			}
		}

		var reqHash [32]byte
		copy(reqHash[:], reqHashBytes)
		state := parsePersistedIdempotencyState(entry.State)
		if state == idempotencyStateInProgress {
			// A restarted daemon cannot prove whether work was still executing, but it can
			// prove the request was durably accepted and must not silently disappear.
			state = idempotencyStateAccepted
		}
		updatedAt := time.Unix(0, entry.UpdatedAtUnixNano)
		if entry.UpdatedAtUnixNano == 0 {
			updatedAt = time.Unix(0, entry.CreatedAtUnixNano)
		}
		c.entries[key] = &idempotencyEntry{
			createdAt: time.Unix(0, entry.CreatedAtUnixNano),
			updatedAt: updatedAt,
			reqHash:   reqHash,
			state:     state,
			result: idempotencyResult{
				status: entry.Status,
				body:   body,
			},
		}
	}
	c.pruneLocked(now)
	c.enforceCapLocked()
}

// snapshotForPersist marshals current entries under the lock and returns
// the serialized bytes plus the persist path. Caller must hold c.mu.
func (c *idempotencyCache) snapshotForPersist() ([]byte, string) {
	if c.persistPath == "" {
		return nil, ""
	}

	persisted := persistedIdempotencyState{
		Entries: make(map[string]persistedIdempotencyEntry),
	}
	for key, entry := range c.entries {
		if entry == nil {
			continue
		}
		persisted.Entries[key] = persistedIdempotencyEntry{
			CreatedAtUnixNano: entry.createdAt.UnixNano(),
			UpdatedAtUnixNano: entry.updatedAt.UnixNano(),
			ReqHashHex:        hex.EncodeToString(entry.reqHash[:]),
			State:             string(entry.state),
			Status:            entry.result.status,
			BodyBase64:        base64.StdEncoding.EncodeToString(entry.result.body),
		}
	}
	data, _ := json.Marshal(persisted)
	return data, c.persistPath
}

func writePersistData(data []byte, path string) error {
	if path == "" || data == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (e *idempotencyEntry) isTerminal() bool {
	return e != nil && (e.state == idempotencyStateCompleted || e.state == idempotencyStateFailed)
}

func parsePersistedIdempotencyState(value string) idempotencyState {
	switch idempotencyState(value) {
	case idempotencyStateAccepted, idempotencyStateInProgress, idempotencyStateCompleted, idempotencyStateFailed:
		return idempotencyState(value)
	default:
		return idempotencyStateCompleted
	}
}
