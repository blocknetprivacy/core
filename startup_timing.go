package main

import (
	"log"
	"time"
)

// processStartedAt approximates process start, for the time-to-API log.
var processStartedAt = time.Now()

// logSinceProcessStart logs a startup milestone relative to process start.
func logSinceProcessStart(milestone string) {
	log.Printf("[perf] startup milestone=%s since_process_start=%s", milestone, time.Since(processStartedAt).Round(time.Millisecond))
}

// startupTimer logs how long each daemon startup stage takes, so slow
// restarts (which stall pool templates and block submits) can be traced to a
// stage.
type startupTimer struct {
	startedAt time.Time
	last      time.Time
}

func newStartupTimer() *startupTimer {
	now := time.Now()
	return &startupTimer{startedAt: now, last: now}
}

func (t *startupTimer) stage(name string) {
	now := time.Now()
	log.Printf("[perf] startup stage=%s elapsed=%s", name, now.Sub(t.last).Round(time.Millisecond))
	t.last = now
}

func (t *startupTimer) done() {
	log.Printf("[perf] startup stage=new_daemon_total elapsed=%s", time.Since(t.startedAt).Round(time.Millisecond))
}
