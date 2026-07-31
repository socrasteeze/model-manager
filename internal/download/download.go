// Package download fetches model files from Civitai and HuggingFace.
//
// Spec §15 promotes this out of the last phase because it is the single most
// common reason people run Stability Matrix's model manager, and calls it its
// own workstream rather than a one-liner. The parts that make it one:
// auth-gated models, rate limiting, resumable multi-GB transfers, partial-file
// quarantine, and checksum verification before the file is admitted to the
// index.
//
// Job lifecycle:
//
//	pending → downloading → verifying → complete
//	              |             |→ quarantined  (hash/size mismatch; partial moved aside)
//	              |→ failed                      (partial kept — resumable)
//	              |→ cancelled                   (partial kept — resumable)
//
// A job is in flight while pending, downloading or verifying; starting the
// same ID again during that window fails with ErrInFlight. Once terminal, the
// same ID may be started again: the record is replaced and the transfer
// resumes from whatever partial remains. The ID stays deterministic
// (hash of URL+filename) because it names the .part file that resume needs.
package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrChecksumMismatch means the bytes that arrived are not the bytes that were
// promised.
var ErrChecksumMismatch = errors.New("download: checksum mismatch")

// ErrSizeMismatch means the byte count differs from what the listing promised.
// Only checked when no expected hash is available: a hash subsumes it.
var ErrSizeMismatch = errors.New("download: size mismatch")

// ErrNoSpace is a disk-full condition, worth distinguishing because the fix is
// not "retry".
var ErrNoSpace = errors.New("download: no space left on device")

// ErrInFlight means this job ID is already being transferred. The caller gets
// the current job snapshot alongside it, so an HTTP layer can answer with the
// in-progress state rather than an opaque refusal.
var ErrInFlight = errors.New("download: job already in flight")

// State is where a job is.
type State string

const (
	StatePending     State = "pending"
	StateDownloading State = "downloading"
	StateVerifying   State = "verifying"
	StateComplete    State = "complete"
	StateFailed      State = "failed"
	StateQuarantined State = "quarantined"
	StateCancelled   State = "cancelled"
)

// terminal reports whether a state is final.
func terminal(s State) bool {
	switch s {
	case StateComplete, StateFailed, StateQuarantined, StateCancelled:
		return true
	}
	return false
}

// Job is one download.
type Job struct {
	ID  string `json:"id"`
	URL string `json:"url"`

	// DestDir and Filename are where the file lands once it is verified.
	DestDir  string `json:"dest_dir"`
	Filename string `json:"filename"`

	// DestRoot is the canonical scanned root DestDir sits under. Set by the
	// API layer from its allowlist match, and used when indexing the finished
	// file — recording the caller's raw spelling instead would fork the root
	// string in the database (trailing slash, case, symlinks).
	DestRoot string `json:"dest_root,omitempty"`

	// ExpectedSHA256, when known, is checked before the file is admitted. An
	// empty value means the download is accepted on arrival, which is weaker and
	// is reported as such.
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	ExpectedSize   int64  `json:"expected_size,omitempty"`

	State      State  `json:"state"`
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total"`
	Error      string `json:"error,omitempty"`
	ActualSHA  string `json:"actual_sha256,omitempty"`
	FinalPath  string `json:"final_path,omitempty"`

	// QuarantinePath is where rejected bytes were moved. Quarantine means "not
	// admitted", not "destroyed" — but the .part name must be freed, or every
	// retry of this URL would resume from the poisoned bytes forever.
	QuarantinePath string `json:"quarantine_path,omitempty"`

	// IndexError records a post-download indexing failure: the file is
	// verified and in place, the library just does not show it yet.
	IndexError string `json:"index_error,omitempty"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// Progress reports transfer state.
func (j *Job) Progress() float64 {
	if j.Total <= 0 {
		return 0
	}
	return float64(j.Downloaded) / float64(j.Total) * 100
}

// Manager runs downloads.
type Manager struct {
	// WorkDir holds partial files. Deliberately not the destination directory:
	// a half-written model sitting in a tool's models folder will be picked up
	// and loaded by that tool, which is the quarantine §15 asks for.
	WorkDir string

	HTTP      *http.Client
	UserAgent string

	// TokenFor returns the bearer credential to send to a URL, or "" for
	// none. Selection is host-scoped by the caller (origin.Client.TokenFor),
	// so a Civitai key is never handed to huggingface.co and vice versa. Nil
	// means every request goes out unauthenticated. A single key field here
	// would be wrong by construction: one Manager serves downloads from
	// several unrelated providers.
	TokenFor func(url string) string

	// OnComplete, when set, runs after a job reaches StateComplete, outside
	// the manager lock. A non-empty return is recorded as the job's
	// IndexError. This is how the daemon indexes a finished file without the
	// download package importing the store.
	OnComplete func(Job) string

	// MaxRetries bounds resume attempts for one job.
	MaxRetries int

	// mu protects jobs, running, and every *Job the maps point at. It is
	// never held across network or disk I/O; transfers mutate their job only
	// through update().
	mu      sync.Mutex
	jobs    map[string]*Job
	running map[string]context.CancelFunc
}

// NewManager returns a Manager storing partials under workDir.
func NewManager(workDir string) (*Manager, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("download: creating work directory: %w", err)
	}
	return &Manager{
		WorkDir: workDir,
		// No overall timeout: a multi-gigabyte transfer over a slow link is the
		// normal case, and a client timeout would cap the file size this tool
		// can fetch. Stalls are caught by the response-header timeout instead.
		HTTP: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       60 * time.Second,
			},
		},
		UserAgent:  "model-manager/1.0 (+https://github.com/socrasteeze/model-manager)",
		MaxRetries: 5,
		jobs:       map[string]*Job{},
		running:    map[string]context.CancelFunc{},
	}, nil
}

// Jobs returns a snapshot of every job, oldest first.
//
// Sorted because the map's iteration order is randomized per call, and a
// polling UI rendering a reshuffled queue once a second reads as broken.
func (m *Manager) Jobs() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool {
		if !out[i].StartedAt.Equal(out[k].StartedAt) {
			return out[i].StartedAt.Before(out[k].StartedAt)
		}
		return out[i].ID < out[k].ID
	})
	return out
}

// Job returns one job's state.
func (m *Manager) Job(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

func (m *Manager) update(id string, fn func(*Job)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		fn(j)
	}
}

// begin claims an ID for a new transfer.
//
// An in-flight duplicate is rejected: two transfers appending to the same
// .part file interleave their streams into byte garbage, so presence in
// `running` is checked and set under one lock acquisition — there is no
// window in which two callers can both claim the ID. A terminal record is
// replaced, which is what makes retry work.
func (m *Manager) begin(job *Job, cancel context.CancelFunc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, inFlight := m.running[job.ID]; inFlight {
		return ErrInFlight
	}
	job.State = StatePending
	job.StartedAt = time.Now()
	m.jobs[job.ID] = job
	m.running[job.ID] = cancel
	return nil
}

// end releases the in-flight claim.
func (m *Manager) end(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, id)
}

// Cancel stops an in-flight job. Returns false for unknown or terminal IDs.
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	cancel, ok := m.running[id]
	m.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// Remove forgets a terminal job. Returns false while in flight — a running
// transfer must be cancelled, not orphaned.
func (m *Manager) Remove(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, inFlight := m.running[id]; inFlight {
		return false
	}
	if _, ok := m.jobs[id]; !ok {
		return false
	}
	delete(m.jobs, id)
	return true
}

// normalize fills the derived job fields.
func normalize(job *Job) error {
	if job.Filename == "" {
		job.Filename = filenameFromURL(job.URL)
	}
	if job.ID == "" {
		job.ID = jobID(job.URL, job.Filename)
	}
	if job.DestDir == "" {
		return errors.New("download: no destination directory")
	}
	return nil
}

// Fetch downloads a file synchronously, verifies it, and moves it into place.
//
// The returned Job is a snapshot of the final state; the file is only at
// FinalPath when State is StateComplete. A duplicate of an in-flight job
// returns that job's current snapshot and ErrInFlight.
func (m *Manager) Fetch(ctx context.Context, job Job) (Job, error) {
	if err := normalize(&job); err != nil {
		return job, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := m.begin(&job, cancel); err != nil {
		current, _ := m.Job(job.ID)
		return current, err
	}
	defer m.end(job.ID)
	return m.run(runCtx, job)
}

// Start registers a job and runs the transfer in the background.
//
// Registration is synchronous: the returned snapshot is StatePending and
// already visible to Jobs()/Job(), so an HTTP caller can hand its client the
// ID before the first byte moves. Without this the client's first poll can
// race the goroutine, see nothing, and give up on polling entirely.
func (m *Manager) Start(ctx context.Context, job Job) (Job, error) {
	if err := normalize(&job); err != nil {
		return job, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	if err := m.begin(&job, cancel); err != nil {
		cancel()
		current, _ := m.Job(job.ID)
		return current, err
	}
	snapshot := job
	go func() {
		defer cancel()
		defer m.end(job.ID)
		m.run(runCtx, job)
	}()
	return snapshot, nil
}

// run is the transfer body shared by Fetch and Start.
func (m *Manager) run(ctx context.Context, job Job) (Job, error) {
	partial := filepath.Join(m.WorkDir, job.ID+".part")

	fail := func(err error) (Job, error) {
		m.update(job.ID, func(j *Job) {
			j.State = StateFailed
			j.Error = err.Error()
			j.FinishedAt = time.Now()
			if errors.Is(err, context.Canceled) {
				j.State = StateCancelled
			}
		})
		final, _ := m.Job(job.ID)
		return final, err
	}

	if err := m.transfer(ctx, job.ID, job.URL, partial); err != nil {
		return fail(err)
	}

	m.update(job.ID, func(j *Job) { j.State = StateVerifying })

	sum, size, err := hashFile(partial)
	if err != nil {
		return fail(err)
	}

	m.update(job.ID, func(j *Job) {
		j.ActualSHA = sum
		j.Downloaded = size
	})

	// The checks that keep a corrupt or substituted file out of the index.
	// ActualSHA is recorded above before the partial is moved, so the
	// evidence of what actually arrived survives quarantine.
	if job.ExpectedSHA256 != "" && !strings.EqualFold(sum, job.ExpectedSHA256) {
		reason := fmt.Sprintf("expected %s, got %s", strings.ToLower(job.ExpectedSHA256), sum)
		return m.reject(job.ID, partial, reason, ErrChecksumMismatch)
	}
	// Without a hash, the promised size is the only independent fact to check
	// against. It catches the classic failure: an HTML interstitial served
	// with a 200 landing on disk under a .safetensors name.
	if job.ExpectedSHA256 == "" && job.ExpectedSize > 0 && size != job.ExpectedSize {
		reason := fmt.Sprintf("expected %d bytes, got %d", job.ExpectedSize, size)
		return m.reject(job.ID, partial, reason, ErrSizeMismatch)
	}

	dest, err := m.publish(partial, job.DestDir, job.Filename)
	if err != nil {
		return fail(err)
	}

	m.update(job.ID, func(j *Job) {
		j.State = StateComplete
		j.FinalPath = dest
		j.FinishedAt = time.Now()
	})

	if m.OnComplete != nil {
		final, _ := m.Job(job.ID)
		if msg := m.OnComplete(final); msg != "" {
			m.update(job.ID, func(j *Job) { j.IndexError = msg })
		}
	}

	final, _ := m.Job(job.ID)
	return final, nil
}

// reject quarantines a partial whose content failed verification.
func (m *Manager) reject(id, partial, reason string, sentinel error) (Job, error) {
	q := m.quarantine(partial, id)
	m.update(id, func(j *Job) {
		j.State = StateQuarantined
		j.Error = reason
		j.QuarantinePath = q
		j.FinishedAt = time.Now()
	})
	final, _ := m.Job(id)
	return final, fmt.Errorf("%w: %s", sentinel, reason)
}

// quarantine moves a rejected partial out of the resume path, keeping the
// bytes for inspection. uniquePath means a second quarantine of the same job
// never overwrites earlier evidence. If even the rename fails, the partial is
// removed: clearing the resume path outranks keeping evidence, because a
// poisoned .part silently corrupts every future retry of this URL.
func (m *Manager) quarantine(partial, id string) string {
	q := uniquePath(filepath.Join(m.WorkDir, id+".quarantine"))
	if err := os.Rename(partial, q); err != nil {
		_ = os.Remove(partial)
		return ""
	}
	return q
}

// transfer downloads to a partial file, resuming where it left off.
func (m *Manager) transfer(ctx context.Context, id, url, partial string) error {
	for attempt := 0; attempt <= m.MaxRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var existing int64
		if info, err := os.Stat(partial); err == nil {
			existing = info.Size()
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", m.UserAgent)
		// Host-scoped: some providers gate models behind an account, and
		// without the right credential the server returns an HTML login page
		// with a 200 that would otherwise be written to disk and named
		// .safetensors. But the credential must match the host — see the
		// TokenFor field.
		if m.TokenFor != nil {
			if tok := m.TokenFor(url); tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		if existing > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", existing))
		}

		resp, err := m.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if attempt == m.MaxRetries {
				return fmt.Errorf("download: %s: %w", url, err)
			}
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return err
			}
			continue
		}

		appending := resp.StatusCode == http.StatusPartialContent
		switch {
		case appending:
			// The server honoured the range; keep what is on disk.
		case resp.StatusCode == http.StatusOK:
			// The server ignored the range and is sending the whole file. Start
			// over rather than appending a second copy onto the first.
			existing = 0
		case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
			// "Content-Range: bytes */<total>" carries the real size. Only a
			// partial that exactly matches it is complete; an oversized one
			// (e.g. an appended error page from an earlier run) must be
			// restarted, not declared done.
			total := totalFromContentRange(resp.Header.Get("Content-Range"))
			resp.Body.Close()
			if total > 0 && total == existing {
				return nil
			}
			if err := os.Truncate(partial, 0); err != nil {
				return fmt.Errorf("download: truncating oversized partial: %w", err)
			}
			continue // consumes an attempt, so a 416 loop still terminates
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			resp.Body.Close()
			return fmt.Errorf(
				"download: %s returned %d — this model may require an API key for its provider",
				url, resp.StatusCode)
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			resp.Body.Close()
			if attempt == m.MaxRetries {
				return fmt.Errorf("download: %s returned %d", url, resp.StatusCode)
			}
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return err
			}
			continue
		default:
			resp.Body.Close()
			return fmt.Errorf("download: %s returned %d", url, resp.StatusCode)
		}

		total := resp.ContentLength
		if appending && total > 0 {
			total += existing
		}
		m.update(id, func(j *Job) {
			if total > 0 {
				j.Total = total
			}
			j.Downloaded = existing
			j.State = StateDownloading
		})

		flags := os.O_CREATE | os.O_WRONLY
		if existing > 0 {
			flags |= os.O_APPEND
		} else {
			flags |= os.O_TRUNC
		}
		f, err := os.OpenFile(partial, flags, 0o644)
		if err != nil {
			resp.Body.Close()
			return fmt.Errorf("download: opening partial file: %w", err)
		}

		written, copyErr := m.copyTracking(ctx, id, f, resp.Body, existing)
		f.Close()
		resp.Body.Close()

		if copyErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isNoSpace(copyErr) {
			return fmt.Errorf("%w: %v", ErrNoSpace, copyErr)
		}
		if attempt == m.MaxRetries {
			return fmt.Errorf("download: after %d bytes: %w", written, copyErr)
		}
		// A dropped connection mid-transfer is the normal case for multi-GB
		// files, and everything already on disk is reused on the next attempt.
		if err := sleepCtx(ctx, backoff(attempt)); err != nil {
			return err
		}
	}
	return fmt.Errorf("download: gave up on %s", url)
}

// totalFromContentRange parses the total from "bytes */<total>".
func totalFromContentRange(v string) int64 {
	rest, ok := strings.CutPrefix(strings.TrimSpace(v), "bytes ")
	if !ok {
		return 0
	}
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(rest[i+1:]), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (m *Manager) copyTracking(ctx context.Context, id string, dst io.Writer, src io.Reader, start int64) (int64, error) {
	buf := make([]byte, 1<<20)
	total := start

	for {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return total, writeErr
			}
			total += int64(n)
			m.update(id, func(j *Job) { j.Downloaded = total })
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

// publish moves a verified file into its destination.
//
// This is the only place the tool creates a file inside a model tree, and §14
// permits it precisely because the destination is user-chosen and the content is
// freshly downloaded rather than something that was already there.
func (m *Manager) publish(partial, destDir, filename string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("download: creating destination: %w", err)
	}

	dest := filepath.Join(destDir, filename)
	// Never overwrite. The fence says this tool does not modify existing files,
	// and a download landing on a name that is already taken is exactly the case
	// where honouring that matters.
	dest = uniquePath(dest)

	if err := os.Rename(partial, dest); err == nil {
		return dest, nil
	}
	// A rename across filesystems fails, which is the common case when the work
	// directory is beside the database and the destination is on the array.
	if err := copyFile(partial, dest); err != nil {
		return "", err
	}
	if err := os.Remove(partial); err != nil {
		// The file is published; a leftover partial is untidy, not a failure.
		_ = err
	}
	return dest, nil
}

func uniquePath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for i := 1; i < 1000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return fmt.Sprintf("%s.%d%s", stem, time.Now().UnixNano(), ext)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".incoming"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// Rename last, so a consuming tool never sees a partially copied model under
	// its final name.
	return os.Rename(tmp, dst)
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	var h hash.Hash = sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// filenameFromURL takes the last path segment.
//
// It parses rather than string-slicing: trimming a trailing slash off
// "https://example.com/" and then searching for the last "/" finds the one in
// the scheme and yields the hostname, which is not a filename.
func filenameFromURL(raw string) string {
	parsed, err := neturl.Parse(raw)
	if err != nil {
		return "download.bin"
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		path = path[i+1:]
	}
	if path == "" {
		return "download.bin"
	}
	return sanitizeFilename(path)
}

// sanitizeFilename strips path separators and traversal from a server-supplied
// name, so a hostile Content-Disposition cannot place a file outside the chosen
// destination.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimLeft(name, ".")
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	if name == "" {
		return "download.bin"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

func jobID(url, filename string) string {
	sum := sha256.Sum256([]byte(url + "\x00" + filename))
	return hex.EncodeToString(sum[:8])
}

// backoffTestHook collapses the retry wait in tests. The production curve is
// unchanged; a test that actually slept through it would take a minute.
var backoffTestHook bool

func backoff(attempt int) time.Duration {
	if backoffTestHook {
		return time.Millisecond
	}
	d := time.Second << attempt
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func isNoSpace(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no space left")
}
