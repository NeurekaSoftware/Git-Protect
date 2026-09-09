// Package scheduler runs the repository backup job on its cron schedule.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/neurekadev/git-backup/internal/config"
	"github.com/neurekadev/git-backup/internal/schedule"
)

// jobName is the one scheduled job of the daemon.
const jobName = "repositories"

// waitSlice bounds how long any single sleep lasts so a cron change mid-wait
// is picked up within a second.
const waitSlice = time.Second

// RunReporter receives scheduler timing for the health endpoint. All methods
// must be safe for concurrent use.
type RunReporter interface {
	SetRunStarted(at time.Time)
	SetRunCompleted(startedAt time.Time, outcome string)
	SetNextRun(at time.Time)
}

// SyncRunner runs one repository backup pass.
type SyncRunner interface {
	Run(ctx context.Context, settings *config.Settings) error
}

// RetentionRunner runs one retention pass.
type RetentionRunner interface {
	Run(ctx context.Context, settings *config.Settings) error
}

// Scheduler drives the repository job: it resolves the (hot-reloadable) cron
// expression, waits for the next occurrence in one-second slices, runs the
// sync, then runs retention.
type Scheduler struct {
	getSettings    func() *config.Settings
	repositorySync SyncRunner
	retention      RetentionRunner
	reporter       RunReporter
}

// NewScheduler wires the scheduler. reporter may be nil.
func NewScheduler(
	getSettings func() *config.Settings,
	repositorySync SyncRunner,
	retention RetentionRunner,
	reporter RunReporter,
) *Scheduler {
	return &Scheduler{
		getSettings:    getSettings,
		repositorySync: repositorySync,
		retention:      retention,
		reporter:       reporter,
	}
}

// scheduleLogState carries what the loop has already reported, so an unchanged
// or still-invalid schedule does not repeat the same line on every pass.
type scheduleLogState struct {
	lastAppliedCron string
	lastInvalidCron string
}

// RunForever loops until the context is cancelled. Job failures are swallowed
// (logged) so one bad run never stops the schedule; cancellation exits cleanly
// with a nil error.
func (s *Scheduler) RunForever(ctx context.Context) error {
	slog.Info("Starting scheduled repository job loop.")
	return s.runScheduledLoop(ctx, func() string {
		return s.getSettings().Schedule.Repositories.Cron
	})
}

func (s *Scheduler) runScheduledLoop(ctx context.Context, getCronExpression func() string) error {
	logState := &scheduleLogState{}

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		cronExpression := getCronExpression()

		parsed, ok := s.resolveSchedule(ctx, cronExpression, logState)
		if !ok {
			continue
		}

		nextOccurrence, stopped := s.resolveNextOccurrence(parsed, cronExpression)
		if stopped {
			return nil
		}
		if s.reporter != nil {
			s.reporter.SetNextRun(nextOccurrence)
		}

		switch s.delayUntil(ctx, nextOccurrence, cronExpression, getCronExpression) {
		case waitCancelled:
			return nil
		case waitReschedule:
			slog.Info(fmt.Sprintf("%s: schedule changed from '%s' to '%s'. Recomputing next run.",
				jobName, cronExpression, getCronExpression()))
			continue
		}

		if !s.runWithTiming(ctx) {
			return nil
		}

		if ctx.Err() == nil {
			s.runRetention(ctx)
		}
	}
}

// resolveSchedule parses the configured schedule, reporting a change or an
// invalid expression only the first time each is seen. It returns false when
// the expression is unusable, having waited a beat so the caller can simply
// retry until the settings are fixed.
func (s *Scheduler) resolveSchedule(ctx context.Context, cronExpression string, logState *scheduleLogState) (schedule.Schedule, bool) {
	slog.Debug(fmt.Sprintf("%s: evaluating cron expression '%s'.", jobName, cronExpression))

	parsed, err := schedule.Parse(cronExpression)
	if err != nil {
		if logState.lastInvalidCron != cronExpression {
			slog.Warn(fmt.Sprintf("%s: schedule '%s' is invalid (%v). Waiting for configuration reload.",
				jobName, cronExpression, err))
			logState.lastInvalidCron = cronExpression
		}

		select {
		case <-ctx.Done():
			return schedule.Schedule{}, false
		case <-time.After(waitSlice):
		}
		return schedule.Schedule{}, false
	}

	logState.lastInvalidCron = ""
	if logState.lastAppliedCron != cronExpression {
		slog.Info(fmt.Sprintf("%s: active schedule is '%s'.", jobName, cronExpression))
		logState.lastAppliedCron = cronExpression
	}

	return parsed, true
}

// resolveNextOccurrence reports when the job runs next.
func (s *Scheduler) resolveNextOccurrence(parsed schedule.Schedule, cronExpression string) (time.Time, bool) {
	now := time.Now()
	next := parsed.Next(now.Add(time.Millisecond))

	if next.IsZero() {
		slog.Error(fmt.Sprintf("%s: schedule '%s' has no next occurrence. Stopping this job loop.",
			jobName, cronExpression))
		return time.Time{}, true
	}

	secondsUntilNextRun := max(0, int64(math.Ceil(next.Sub(now).Seconds())))
	slog.Info(fmt.Sprintf("%s: next run at %s (in %s).",
		jobName, schedule.FormatTimestamp(next), schedule.DurationShort(secondsUntilNextRun)))

	return next, false
}

type waitResult int

const (
	waitTargetReached waitResult = iota
	waitReschedule
	waitCancelled
)

// delayUntil waits for the target in one-second slices, re-checking the cron
// expression on every slice so a hot-reloaded schedule is honored without
// waiting out the old one.
func (s *Scheduler) delayUntil(ctx context.Context, target time.Time, scheduledCronExpression string, getCronExpression func() string) waitResult {
	for {
		if err := ctx.Err(); err != nil {
			return waitCancelled
		}

		if current := getCronExpression(); current != scheduledCronExpression {
			slog.Debug(fmt.Sprintf("%s: detected schedule change while waiting (old='%s', new='%s').",
				jobName, scheduledCronExpression, current))
			return waitReschedule
		}

		remaining := time.Until(target)
		if remaining <= 0 {
			return waitTargetReached
		}

		delay := waitSlice
		if remaining < delay {
			delay = remaining
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return waitCancelled
		case <-timer.C:
		}
	}
}

// runWithTiming runs the job inside its timing envelope, reporting the outcome
// to the health reporter. It returns false when the run was cancelled and the
// loop should exit.
func (s *Scheduler) runWithTiming(ctx context.Context) bool {
	runStartedAt := time.Now()
	// The job service logs its own richer "started" line (with counts); keep
	// only the timing envelope here to avoid a duplicate start marker for the
	// same event.
	slog.Debug(fmt.Sprintf("%s: run started.", jobName))
	if s.reporter != nil {
		s.reporter.SetRunStarted(runStartedAt)
	}

	err := s.repositorySync.Run(ctx, s.getSettings())
	if err != nil && ctx.Err() == nil {
		slog.Error(fmt.Sprintf("%s: run failed after %s seconds. error=%s.",
			jobName, formatSeconds(time.Since(runStartedAt).Seconds()), err.Error()))
		if s.reporter != nil {
			s.reporter.SetRunCompleted(runStartedAt, "failed")
		}
		return true
	}
	if ctx.Err() != nil {
		return false
	}

	slog.Info(fmt.Sprintf("%s: run completed in %s seconds.",
		jobName, formatSeconds(time.Since(runStartedAt).Seconds())))
	if s.reporter != nil {
		s.reporter.SetRunCompleted(runStartedAt, "success")
	}
	return true
}

func (s *Scheduler) runRetention(ctx context.Context) {
	err := s.retention.Run(ctx, s.getSettings())
	if err == nil {
		return
	}
	if ctx.Err() != nil {
		// Exit cleanly on shutdown.
		slog.Warn("Retention cancelled because shutdown was requested.")
		return
	}
	slog.Error(fmt.Sprintf("Retention failed after the %s job run. error=%s.", jobName, err.Error()))
}

// formatSeconds renders a duration the way the scheduler announcements always
// have: rounded to three decimals with trailing zeros trimmed.
func formatSeconds(seconds float64) string {
	return strconv.FormatFloat(math.Round(seconds*1000)/1000, 'f', -1, 64)
}
