package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/yuuki/grafana-llm-studio-app/pkg/jobs"
)

const (
	defaultSSEHeartbeatInterval = 15 * time.Second
	sseContentType              = "text/event-stream; charset=utf-8"
)

// splitHeaderOutsideQuotes splits a comma-separated HTTP header value without
// splitting on commas inside double-quoted parameter strings (RFC 9110).
func splitHeaderOutsideQuotes(s string) []string {
	var parts []string
	start := 0
	inQuotes := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inQuotes {
			escaped = true
			continue
		}
		if c == '"' {
			inQuotes = !inQuotes
			continue
		}
		if c == ',' && !inQuotes {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// wantsEventStream checks whether the HTTP Accept header explicitly accepts text/event-stream
// with a valid non-zero quality weight (0 < q <= 1) per RFC 9110.
func wantsEventStream(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, headerVal := range r.Header.Values("Accept") {
		for _, rawRange := range splitHeaderOutsideQuotes(headerVal) {
			rawRange = strings.TrimSpace(rawRange)
			if rawRange == "" {
				continue
			}
			mediaType, params, err := mime.ParseMediaType(rawRange)
			if err != nil || mediaType != "text/event-stream" {
				continue
			}
			if qStr, hasQ := params["q"]; hasQ {
				q, err := strconv.ParseFloat(strings.TrimSpace(qStr), 64)
				if err != nil || math.IsNaN(q) || math.IsInf(q, 0) || q <= 0 || q > 1 {
					continue
				}
			}
			return true
		}
	}
	return false
}

// sseStreamWriter serializes application-level SSE stage events, periodic keepalive comments,
// and the single terminal result/error event over Grafana CallResource http.Flusher.
//
// Headers are deferred until the first StageEvent so pre-flight errors (400/401/403/404/409/422/429/503)
// still return standard HTTP status codes and JSON error payloads.
type sseStreamWriter struct {
	ctx               context.Context
	w                 http.ResponseWriter
	flusher           http.Flusher
	heartbeatInterval time.Duration

	mu       sync.Mutex
	started  bool
	closed   bool
	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

func newSSEStreamWriter(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, heartbeatInterval time.Duration) *sseStreamWriter {
	return &sseStreamWriter{
		ctx:               ctx,
		w:                 w,
		flusher:           flusher,
		heartbeatInterval: heartbeatInterval,
		stopCh:            make(chan struct{}),
	}
}

func (s *sseStreamWriter) signalStop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}

func (s *sseStreamWriter) startLocked() {
	if s.started {
		return
	}
	h := s.w.Header()
	h.Set("Content-Type", sseContentType)
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	h.Set("Vary", "Accept")
	s.w.WriteHeader(http.StatusOK)
	s.started = true

	if s.heartbeatInterval > 0 {
		s.wg.Add(1)
		go s.runHeartbeat()
	}
}

func (s *sseStreamWriter) runHeartbeat() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			if s.closed || !s.started || s.ctx.Err() != nil {
				s.mu.Unlock()
				return
			}
			if _, err := s.w.Write([]byte(": keepalive\n\n")); err != nil {
				s.closed = true
				s.signalStop()
				s.mu.Unlock()
				return
			}
			s.flusher.Flush()
			s.mu.Unlock()
		}
	}
}

// StopHeartbeat terminates the background keepalive goroutine and waits for it to exit
// before the HTTP handler returns.
func (s *sseStreamWriter) StopHeartbeat() {
	s.signalStop()
	s.wg.Wait()
}

// EmitStage flushes a single application-level StageEvent frame immediately.
func (s *sseStreamWriter) EmitStage(ev jobs.StageEvent) {
	raw, err := json.Marshal(ev)
	if err != nil {
		log.DefaultLogger.Error("failed to marshal SSE stage event", "err", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return
	}
	s.startLocked()
	s.writeEventLocked("stage", raw)
}

// FinishResult writes the final validated JobSnapshot as an SSE `result` event,
// or falls back to standard application/json if no SSE stage frame was emitted.
func (s *sseStreamWriter) FinishResult(snap jobs.JobSnapshot) {
	s.StopHeartbeat()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true

	if !s.started {
		s.w.Header().Set("Vary", "Accept")
		writeJSON(s.w, http.StatusOK, snap)
		return
	}
	if s.ctx.Err() != nil {
		return
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		log.DefaultLogger.Error("failed to marshal SSE result event", "err", err)
		return
	}
	s.writeEventLocked("result", raw)
}

// FinishError writes a sanitized job error either as a standard HTTP JSON error
// (if no SSE frame has been flushed yet) or as a terminal SSE `error` event.
func (s *sseStreamWriter) FinishError(err error, snap jobs.JobSnapshot) {
	s.StopHeartbeat()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true

	if !s.started {
		s.w.Header().Set("Vary", "Accept")
		writeJobError(s.w, err, snap)
		return
	}
	if s.ctx.Err() != nil {
		return
	}
	status, payload := buildJobErrorPayload(err, snap)
	payload["status"] = status
	raw, mErr := json.Marshal(payload)
	if mErr != nil {
		log.DefaultLogger.Error("failed to marshal SSE error event", "err", mErr)
		return
	}
	s.writeEventLocked("error", raw)
}

func (s *sseStreamWriter) writeEventLocked(eventName string, jsonPayload []byte) {
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventName, jsonPayload); err != nil {
		s.closed = true
		s.signalStop()
		return
	}
	s.flusher.Flush()
}
