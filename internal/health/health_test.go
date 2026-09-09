package health

import (
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestReportReflectsRunOutcome(t *testing.T) {
	server := NewServer("localhost", 0)
	// Start with port 0 (ephemeral) by binding manually through Start's
	// listener: Start uses the configured port, so rebind on an ephemeral one.
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	server.listener = listener
	server.httpServer = &http.Server{Handler: http.HandlerFunc(server.handle)}
	go func() { _ = server.httpServer.Serve(listener) }()
	t.Cleanup(server.Stop)
	base := "http://" + listener.Addr().String()

	fetch := func() Report {
		response, err := http.Get(base + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		var report Report
		if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
			t.Fatal(err)
		}
		return report
	}

	report := fetch()
	if report.Status != "ok" {
		t.Errorf("initial status = %q, want ok", report.Status)
	}
	if report.LastRunOutcome != nil {
		t.Errorf("initial outcome = %v, want null", *report.LastRunOutcome)
	}
	if report.NextRunAt != nil {
		t.Errorf("initial next run = %v, want null", *report.NextRunAt)
	}
	if _, err := time.Parse(time.RFC3339, report.StartedAt); err != nil {
		t.Errorf("startedAt = %q is not RFC3339: %v", report.StartedAt, err)
	}

	started := time.Now().Add(-2 * time.Second)
	server.SetRunStarted(started)
	server.SetRunCompleted(started, "failed")
	next := time.Now().Add(time.Minute)
	server.SetNextRun(next)

	report = fetch()
	if report.Status != "degraded" {
		t.Errorf("status after a failed run = %q, want degraded", report.Status)
	}
	if report.LastRunOutcome == nil || *report.LastRunOutcome != "failed" {
		t.Errorf("outcome = %v, want failed", report.LastRunOutcome)
	}
	if report.LastRunStartedAt == nil || report.LastRunCompletedAt == nil {
		t.Error("run timestamps should be set")
	}
	if report.LastRunDurationSeconds == nil || *report.LastRunDurationSeconds < 1 {
		t.Errorf("duration = %v, want the measured span", report.LastRunDurationSeconds)
	}
	if report.NextRunAt == nil {
		t.Error("next run should be reported")
	}

	// A succeeding run clears the degraded state.
	server.SetRunStarted(started)
	server.SetRunCompleted(started, "success")
	if report = fetch(); report.Status != "ok" {
		t.Errorf("status after a successful run = %q, want ok", report.Status)
	}
}

func TestStopIsSafeWhenNeverStarted(t *testing.T) {
	server := NewServer("localhost", 0)
	server.Stop()
}
