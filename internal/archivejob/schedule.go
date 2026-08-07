package archivejob

// The watchlist timer.
//
// This is the first background timer in the program. Everything else here is
// request-driven, and serve.go has had exactly one goroutine -- the HTTP server.
// So the interesting part is not the ticker but the four ways a timer must not
// misbehave when nobody is watching it:
//
//  1. it must not fire at start-up, or a crash-looping service sweeps on every
//     restart and several machines on one network synchronise against the same
//     provider;
//  2. it must go through Start rather than around it, so it obeys the same
//     at-most-one and shared-throttle rules a human-triggered run does;
//  3. it must back off while nothing can be reached, rather than spending the
//     same failing requests on a fixed schedule forever;
//  4. it must stop when the daemon stops, on the same signal path everything
//     else uses, without a second shutdown mechanism to get wrong.

import (
	"context"
	"math/rand"
	"time"

	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/store"
)

const (
	// maxBackoff bounds the offline slowdown. A day is long enough that an
	// unplugged machine costs almost nothing, and short enough that a network
	// that came back overnight is noticed the next morning.
	maxBackoff = 24 * time.Hour

	// watchBatch caps how many watched models one tick looks at, so a tick is
	// bounded work rather than a whole watchlist.
	watchBatch = 25
)

// Scheduler drives the watchlist on a timer.
type Scheduler struct {
	m        *Manager
	interval time.Duration
	stopped  chan struct{}

	// consecutiveFailures drives the backoff. One int rather than a state
	// machine: the only thing worth remembering is how long to wait next.
	consecutiveFailures int

	// ticks counts elapsed waits, so the first can be shorter than the rest.
	ticks int
}

// NewScheduler builds a scheduler. An interval of zero disables it.
func NewScheduler(m *Manager, interval time.Duration) *Scheduler {
	return &Scheduler{m: m, interval: interval, stopped: make(chan struct{})}
}

// Stopped closes when Run has returned, so shutdown can wait for it.
func (s *Scheduler) Stopped() <-chan struct{} { return s.stopped }

// Run drives the watchlist until ctx is cancelled. Blocks; run it in a goroutine.
func (s *Scheduler) Run(ctx context.Context) {
	defer close(s.stopped)
	if s.interval <= 0 {
		return
	}

	for {
		wait := s.nextDelay()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		s.ticks++
		s.tick(ctx)
	}
}

// nextDelay is how long to wait before the next tick.
//
// The first wait is half an interval plus a random half, so a daemon that
// restarts often neither sweeps on every start nor never reaches its first tick,
// and several machines brought up by the same script do not line up against the
// same provider. Later waits are a full interval ±10%, for the drift the same
// reasoning wants. Both are multiplied by the offline backoff.
func (s *Scheduler) nextDelay() time.Duration {
	base := s.interval
	for i := 0; i < s.consecutiveFailures && base < maxBackoff; i++ {
		base *= 2
	}
	if base > maxBackoff {
		base = maxBackoff
	}

	if s.ticks == 0 {
		return base/2 + time.Duration(rand.Int63n(int64(base/2)+1))
	}
	// ±10%, as an offset from -10% across a 20% span.
	spread := int64(base / 5)
	return base - base/10 + time.Duration(rand.Int63n(spread+1))
}

// tick runs one watchlist pass.
//
// A tick that cannot run is skipped, not queued. A scheduler that waited its
// turn behind a manual sweep would fire the moment that sweep ended, which is
// the one time the work is least likely to be needed -- and skipping costs at
// most one interval.
func (s *Scheduler) tick(ctx context.Context) {
	if _, running := s.m.InFlight(); running {
		return
	}
	if s.m.busy != nil {
		if _, _, busy := s.m.busy(); busy {
			return
		}
	}

	targets, reachable, err := s.m.dueTargets(ctx, watchBatch)
	switch {
	case err != nil || !reachable:
		// Offline, or the provider refused. Slow down rather than spending the
		// same failing requests on the same schedule forever.
		s.consecutiveFailures++
		return
	default:
		s.consecutiveFailures = 0
	}
	if len(targets) == 0 {
		return
	}

	// Through Start, never around it, so a scheduled run obeys the same
	// at-most-one and shared-throttle rules a human-triggered one does.
	if _, err := s.m.Start(Options{
		Targets:       targets,
		StartDownload: s.m.autoDownload,
	}); err != nil {
		return
	}
}

// dueTargets asks each watched model whether it has a version we do not have.
//
// reachable is returned separately from err because the two mean different
// things to the caller: an unreachable provider is a reason to back off, and a
// database error is not.
func (m *Manager) dueTargets(ctx context.Context, limit int) (targets []Target, reachable bool, err error) {
	watches, err := m.st.ArchiveWatches(limit)
	if err != nil {
		return nil, true, err
	}
	if len(watches) == 0 {
		return nil, true, nil
	}

	client := m.client()
	for _, w := range watches {
		if ctx.Err() != nil {
			break
		}
		// Recorded as checked whatever the answer, so a run cut short leaves the
		// models it reached at the back of the queue and the next tick continues
		// rather than starting over at the same head.
		latest, err := client.LatestVersion(ctx, w.ModelID)
		if err != nil {
			// The first failure decides: nothing here is worth retrying inside
			// one tick, and the backoff exists for exactly this.
			return targets, false, nil
		}
		if markErr := m.st.MarkArchiveWatchChecked(w.Provider, w.ModelID); markErr != nil {
			return targets, true, markErr
		}
		if latest == nil || latest.VersionID == "" {
			// Gone upstream. Recorded so the update sweep stops asking too.
			_ = m.st.MarkOriginModelGone(w.Provider, w.ModelID)
			continue
		}

		item, err := m.st.ArchiveItemFor(w.Provider, w.ModelID, latest.VersionID)
		if err != nil {
			return targets, true, err
		}
		// Already have it, and completely: nothing to do. An incomplete one is
		// re-queued, which is what makes a partial archive finish itself.
		if item != nil && item.Complete() {
			continue
		}
		if item == nil && !w.AutoPull {
			// A new version on a watch that only reports. The row is created so
			// the UI can offer it, but nothing is fetched.
			if err := m.st.PutArchiveItem(store.ArchiveItem{
				Provider: w.Provider, ModelID: w.ModelID, VersionID: latest.VersionID,
			}); err != nil {
				return targets, true, err
			}
			continue
		}
		targets = append(targets, Target{
			Provider: w.Provider, ModelID: w.ModelID, VersionID: latest.VersionID,
		})
	}
	return targets, true, nil
}

// SetHooks wires what the scheduler needs from the daemon.
//
// busy asks the shared-throttle group, and autoDownload hands a file to the
// download queue. Both are functions rather than dependencies so this package
// does not import the API layer that constructs it.
func (m *Manager) SetHooks(busy func() (string, string, bool),
	autoDownload func(Target, origin.RemoteFile) error) {
	m.busy = busy
	m.autoDownload = autoDownload
}
