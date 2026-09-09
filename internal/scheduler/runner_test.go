package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/neurekadev/git-backup/internal/config"
)

// fakeRunner records sync and retention runs and can fail on demand.
type fakeRunner struct {
	mu      sync.Mutex
	calls   int
	runErr  error
	onCalls map[int]error
}

func (f *fakeRunner) Run(_ context.Context, _ *config.Settings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.onCalls != nil {
		if err, scheduled := f.onCalls[f.calls]; scheduled {
			return err
		}
	}
	return f.runErr
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type recordingReporter struct {
	mu       sync.Mutex
	outcomes []string
	nextRuns int
}

func (r *recordingReporter) SetRunStarted(time.Time) {}

func (r *recordingReporter) SetRunCompleted(_ time.Time, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, outcome)
}

func (r *recordingReporter) SetNextRun(time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextRuns++
}

// cronSettings returns a settings value whose schedule can be swapped at
// runtime, mirroring the hot-reload the scheduler must honor.
func cronSettings(getCron func() string) func() *config.Settings {
	return func() *config.Settings {
		return &config.Settings{
			Schedule: config.Schedule{Repositories: config.JobSchedule{Cron: getCron()}},
		}
	}
}

func TestSchedulerRunsOnCronAndCancelsCleanly(t *testing.T) {
	// The six-field expression with a wildcard seconds field fires every
	// second, keeping the test fast.
	syncRunner := &fakeRunner{}
	retentionRunner := &fakeRunner{}
	reporter := &recordingReporter{}

	currentCron := "* * * * * *"
	schedulerService := NewScheduler(cronSettings(func() string { return currentCron }), syncRunner, retentionRunner, reporter)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- schedulerService.RunForever(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && syncRunner.count() < 2 {
		time.Sleep(50 * time.Millisecond)
	}
	if syncRunner.count() < 2 {
		t.Fatalf("sync runs = %d, want at least 2 within the deadline", syncRunner.count())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunForever should end cleanly on cancellation, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunForever did not exit after cancellation")
	}

	// Retention runs after each successful sync pass.
	if retentionRunner.count() < 1 {
		t.Errorf("retention runs = %d, want at least 1", retentionRunner.count())
	}
	if len(reporter.outcomes) < 2 {
		t.Errorf("reported outcomes = %v", reporter.outcomes)
	}
	for _, outcome := range reporter.outcomes {
		if outcome != "success" {
			t.Errorf("unexpected outcome %q", outcome)
		}
	}
}

func TestSchedulerSwallowsJobErrorsAndKeepsRunning(t *testing.T) {
	syncRunner := &fakeRunner{runErr: errors.New("storage unavailable")}
	retentionRunner := &fakeRunner{}
	schedulerService := NewScheduler(cronSettings(func() string { return "* * * * * *" }), syncRunner, retentionRunner, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- schedulerService.RunForever(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && syncRunner.count() < 2 {
		time.Sleep(50 * time.Millisecond)
	}
	if syncRunner.count() < 2 {
		t.Fatalf("sync runs = %d, want the loop to continue past failures", syncRunner.count())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunForever did not exit after cancellation")
	}
}

func TestSchedulerRetriesInvalidCronUntilFixed(t *testing.T) {
	mu := sync.Mutex{}
	cron := "not a cron"
	syncRunner := &fakeRunner{}
	retentionRunner := &fakeRunner{}
	schedulerService := NewScheduler(cronSettings(func() string {
		mu.Lock()
		defer mu.Unlock()
		return cron
	}), syncRunner, retentionRunner, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- schedulerService.RunForever(ctx) }()

	time.Sleep(1200 * time.Millisecond)
	if syncRunner.count() != 0 {
		t.Fatalf("sync runs with an invalid schedule = %d, want 0", syncRunner.count())
	}

	mu.Lock()
	cron = "* * * * * *"
	mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && syncRunner.count() < 1 {
		time.Sleep(50 * time.Millisecond)
	}
	if syncRunner.count() < 1 {
		t.Fatal("the scheduler should start running once the cron is fixed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunForever did not exit after cancellation")
	}
}

func TestSchedulerReschedulesMidWait(t *testing.T) {
	// Start with a schedule that will not fire within the test, then flip to
	// an every-second schedule: the mid-wait change must be honored.
	mu := sync.Mutex{}
	cron := "0 0 1 1 * *"
	syncRunner := &fakeRunner{}
	retentionRunner := &fakeRunner{}
	schedulerService := NewScheduler(cronSettings(func() string {
		mu.Lock()
		defer mu.Unlock()
		return cron
	}), syncRunner, retentionRunner, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- schedulerService.RunForever(ctx) }()

	time.Sleep(1500 * time.Millisecond)
	mu.Lock()
	cron = "* * * * * *"
	mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && syncRunner.count() < 1 {
		time.Sleep(50 * time.Millisecond)
	}
	if syncRunner.count() < 1 {
		t.Fatal("the scheduler should pick up the new schedule without waiting out the old one")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunForever did not exit after cancellation")
	}
}

func TestSchedulerStopsWhenScheduleNeverFires(t *testing.T) {
	// February 30th never exists, so the schedule can never fire again and the
	// loop stops cleanly (matching the previous daemon's behavior).
	syncRunner := &fakeRunner{}
	schedulerService := NewScheduler(cronSettings(func() string { return "0 0 30 2 *" }), syncRunner, &fakeRunner{}, nil)

	done := make(chan error, 1)
	go func() { done <- schedulerService.RunForever(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a never-firing schedule should stop the loop cleanly, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunForever should return promptly when the schedule can never fire")
	}
}
