// Package health serves a small HTTP status endpoint so a stalled scheduler is
// visible from the outside. It is best-effort by design: a bind failure is
// logged and the daemon runs on without it.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Server is the status listener. All mutators are safe for concurrent use; the
// scheduler updates run state while HTTP handlers read it.
type Server struct {
	bind string
	port int

	mu                 sync.Mutex
	startedAt          time.Time
	lastRunStartedAt   *time.Time
	lastRunCompletedAt *time.Time
	lastRunDuration    *float64
	lastRunOutcome     *string // "success" or "failed"
	nextRunAt          *time.Time

	listener   net.Listener
	httpServer *http.Server
}

// NewServer creates the status server bound to bind:port. Call Start to begin
// serving.
func NewServer(bind string, port int) *Server {
	return &Server{bind: bind, port: port, startedAt: time.Now()}
}

// Start binds the listener and serves in a background goroutine. The
// goroutine ends when Stop is called or the listener fails.
func (s *Server) Start() error {
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.bind, s.port))
	if err != nil {
		return err
	}
	s.listener = listener

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handle)
	mux.HandleFunc("/", s.handle)

	s.httpServer = &http.Server{Handler: mux}
	go func() {
		_ = s.httpServer.Serve(listener)
	}()
	return nil
}

// Stop drains the listener. It is safe to call when never started.
func (s *Server) Stop() {
	if s.httpServer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)
	s.httpServer = nil
	s.listener = nil
}

// Restart rebinds the listener when the health settings changed on reload.
func (s *Server) Restart(bind string, port int) {
	s.mu.Lock()
	unchanged := s.httpServer != nil && s.bind == bind && s.port == port
	s.mu.Unlock()
	if unchanged {
		return
	}

	s.Stop()
	s.mu.Lock()
	s.bind = bind
	s.port = port
	s.mu.Unlock()
	if port > 0 {
		if err := s.Start(); err != nil {
			slog.Error("Health listener failed to start. It will stay disabled.", "error", err.Error())
		}
	}
}

func (s *Server) handle(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	report := Report{
		Status:                 "ok",
		StartedAt:              s.startedAt.UTC().Format(time.RFC3339),
		LastRunStartedAt:       formatTime(s.lastRunStartedAt),
		LastRunCompletedAt:     formatTime(s.lastRunCompletedAt),
		LastRunDurationSeconds: s.lastRunDuration,
		LastRunOutcome:         s.lastRunOutcome,
		NextRunAt:              formatTime(s.nextRunAt),
	}
	s.mu.Unlock()

	if report.LastRunOutcome != nil && *report.LastRunOutcome == "failed" {
		report.Status = "degraded"
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}

// Report is the JSON document served on / and /healthz. Monitors keyword-match
// status: "degraded" means the last run failed.
type Report struct {
	Status                 string   `json:"status"`
	StartedAt              string   `json:"startedAt"`
	LastRunStartedAt       *string  `json:"lastRunStartedAt"`
	LastRunCompletedAt     *string  `json:"lastRunCompletedAt"`
	LastRunDurationSeconds *float64 `json:"lastRunDurationSeconds"`
	LastRunOutcome         *string  `json:"lastRunOutcome"`
	NextRunAt              *string  `json:"nextRunAt"`
}

func formatTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}

// SetRunStarted records when a scheduled run began.
func (s *Server) SetRunStarted(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	started := at
	s.lastRunStartedAt = &started
}

// SetRunCompleted records the outcome and duration of a scheduled run.
func (s *Server) SetRunCompleted(startedAt time.Time, outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	completed := time.Now()
	s.lastRunCompletedAt = &completed
	duration := completed.Sub(startedAt).Seconds()
	s.lastRunDuration = &duration
	outcomeCopy := outcome
	s.lastRunOutcome = &outcomeCopy
}

// SetNextRun records the scheduler's next planned occurrence.
func (s *Server) SetNextRun(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := at
	s.nextRunAt = &next
}
