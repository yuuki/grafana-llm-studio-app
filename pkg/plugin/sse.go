package plugin

import (
	"context"
	"encoding/json"
	"fmt"
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

// wantsEventStream checks whether the HTTP Accept header explicitly accepts text/event-stream
// with a non-zero quality weight (RFC 9110).
func wantsEventStream(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, headerVal := range r.Header.Values("Accept") {
		for _, part := range strings.Split(headerVal, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			segments := strings.Split(part, ";")
			mediaType := strings.ToLower(strings.TrimSpace(segments[0]))
			if mediaType != "text/event-stream" {
				continue
			}
			accepted := true
			for _, param := range segments[1:] {
				k, v, ok := strings.Cut(param, "=")
				if !ok {
					continue
				}
				if strings.EqualFold(strings.TrimSpace(k), "q") {
					q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
					if err == nil && q <= 0 {
						accepted = false
					}
				}
			}
			if accepted {
				return true
			}
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
			if !s.closed && s.started && s.ctx.Err() == nil {
				_, _ = s.w.Write([]byte(": keepalive\n\n"))
				s.flusher.Flush()
			}
			s.mu.Unlock()
		}
	}
}

// StopHeartbeat terminates the background keepalive goroutine and waits for it to exit
// before the HTTP handler returns.
func (s *sseStreamWriter) StopHeartbeat() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
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
	_, _ = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventName, jsonPayload)
	s.flusher.Flush()
}
