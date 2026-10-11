package debug

import (
	"log"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Lock tracing is intentionally simple: it's a debug aid to spot mutex contention
// (time spent waiting to acquire locks). It is disabled by default.
//
// Enable with SetLockTrace. Filters default to 0, which logs everything. The
// hold filter only applies to exclusive Lock/Unlock.
//
// Note: For RWMutex read locks (RLock/RUnlock), we can log wait time, but we do
// not try to compute "hold time" because multiple readers may be active at once.

var (
	lockTraceEnabled atomic.Bool

	minWaitNS atomic.Int64
	minHoldNS atomic.Int64

	// Global sequence to make it easier to correlate acquire/release lines for
	// exclusive locks (Mutex + RWMutex write Lock).
	lockSeq atomic.Uint64
)

func SetLockTrace(enabled bool, minWait, minHold time.Duration) {
	minWaitNS.Store(int64(max(minWait, 0)))
	minHoldNS.Store(int64(max(minHold, 0)))
	lockTraceEnabled.Store(enabled)
}

func callerShort(skip int) string {
	// skip=0 => callerShort; skip=1 => lock/unlock wrapper; skip=2 => real callsite
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown:0"
	}
	// Shorten path to last 2 segments when possible.
	parts := strings.Split(file, "/")
	if len(parts) >= 2 {
		file = parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return file + ":" + strconv.Itoa(line)
}

func traceLogf(format string, args ...any) {
	// Standard logger includes date/time; keep this short and grep-friendly.
	log.Printf(format, args...)
}

// RWMutex is a sync.RWMutex with optional contention tracing.
// It is a drop-in replacement for callsites using Lock/Unlock/RLock/RUnlock.
type RWMutex struct {
	mu   sync.RWMutex
	name string

	// Exclusive lock tracking only (best-effort).
	lastWriteAcquireNS atomic.Int64
	lastWriteSeq       atomic.Uint64
}

func NewRWMutex(name string) RWMutex {
	return RWMutex{name: name}
}

func (m *RWMutex) SetName(name string) { m.name = name }

func (m *RWMutex) Lock() {
	if !lockTraceEnabled.Load() {
		m.mu.Lock()
		return
	}

	start := time.Now()
	m.mu.Lock()
	wait := time.Since(start)

	seq := lockSeq.Add(1)
	m.lastWriteSeq.Store(seq)
	m.lastWriteAcquireNS.Store(time.Now().UnixNano())

	if waitNS := int64(wait); waitNS >= minWaitNS.Load() {
		traceLogf("[lock] acquire seq=%d name=%s mode=Lock wait=%s at=%s", seq, m.safeName(), wait.Truncate(time.Microsecond), callerShort(2))
	}
}

func (m *RWMutex) Unlock() {
	if !lockTraceEnabled.Load() {
		m.mu.Unlock()
		return
	}

	seq := m.lastWriteSeq.Load()
	acqNS := m.lastWriteAcquireNS.Load()
	m.mu.Unlock()

	held := time.Since(time.Unix(0, acqNS))
	if heldNS := int64(held); heldNS >= minHoldNS.Load() {
		traceLogf("[lock] release seq=%d name=%s mode=Unlock held=%s at=%s", seq, m.safeName(), held.Truncate(time.Microsecond), callerShort(2))
	}
}

func (m *RWMutex) RLock() {
	if !lockTraceEnabled.Load() {
		m.mu.RLock()
		return
	}

	start := time.Now()
	m.mu.RLock()
	wait := time.Since(start)

	if waitNS := int64(wait); waitNS >= minWaitNS.Load() {
		traceLogf("[lock] acquire name=%s mode=RLock wait=%s at=%s", m.safeName(), wait.Truncate(time.Microsecond), callerShort(2))
	}
}

func (m *RWMutex) RUnlock() {
	if !lockTraceEnabled.Load() {
		m.mu.RUnlock()
		return
	}

	m.mu.RUnlock()
	// RUnlock hold-time isn't tracked; still log release for completeness.
	if minHoldNS.Load() == 0 {
		traceLogf("[lock] release name=%s mode=RUnlock at=%s", m.safeName(), callerShort(2))
	}
}

func (m *RWMutex) safeName() string {
	if m.name == "" {
		return "(unnamed)"
	}
	return m.name
}

// Mutex is a sync.Mutex with optional contention tracing.
type Mutex struct {
	mu   sync.Mutex
	name string

	lastAcquireNS atomic.Int64
	lastSeq       atomic.Uint64
}

func NewMutex(name string) Mutex {
	return Mutex{name: name}
}

func (m *Mutex) SetName(name string) { m.name = name }

func (m *Mutex) Lock() {
	if !lockTraceEnabled.Load() {
		m.mu.Lock()
		return
	}

	start := time.Now()
	m.mu.Lock()
	wait := time.Since(start)

	seq := lockSeq.Add(1)
	m.lastSeq.Store(seq)
	m.lastAcquireNS.Store(time.Now().UnixNano())

	if waitNS := int64(wait); waitNS >= minWaitNS.Load() {
		traceLogf("[lock] acquire seq=%d name=%s mode=Lock wait=%s at=%s", seq, m.safeName(), wait.Truncate(time.Microsecond), callerShort(2))
	}
}

func (m *Mutex) Unlock() {
	if !lockTraceEnabled.Load() {
		m.mu.Unlock()
		return
	}

	seq := m.lastSeq.Load()
	acqNS := m.lastAcquireNS.Load()
	m.mu.Unlock()

	held := time.Since(time.Unix(0, acqNS))
	if heldNS := int64(held); heldNS >= minHoldNS.Load() {
		traceLogf("[lock] release seq=%d name=%s mode=Unlock held=%s at=%s", seq, m.safeName(), held.Truncate(time.Microsecond), callerShort(2))
	}
}

func (m *Mutex) safeName() string {
	if m.name == "" {
		return "(unnamed)"
	}
	return m.name
}
