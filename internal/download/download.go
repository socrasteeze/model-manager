// Package download fetches model files from Civitai and HuggingFace.
//
// Spec §15 promotes this out of the last phase because it is the single most
// common reason people run Stability Matrix's model manager, and calls it its
// own workstream rather than a one-liner. The parts that make it one:
// auth-gated models, rate limiting, resumable multi-GB transfers, partial-file
// quarantine, and checksum verification before the file is admitted to the
// index.
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
	"strings"
	"sync"
	"time"
)

// ErrChecksumMismatch means the bytes that arrived are not the bytes that were
// promised.
var ErrChecksumMismatch = errors.New("download: checksum mismatch")

// ErrNoSpace is a disk-full condition, worth distinguishing because the fix is
// not "retry".
var ErrNoSpace = errors.New("download: no space left on device")

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

// Job is one download.
type Job struct {
	ID  string `json:"id"`
	URL string `json:"url"`

	// DestDir and Filename are where the file lands once it is verified.
	DestDir  string `json:"dest_dir"`
	Filename string `json:"filename"`

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
	APIKey    string
	UserAgent string

	// MaxRetries bounds resume attempts for one job.
	MaxRetries int

	mu   sync.Mutex
	jobs map[string]*Job
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
	}, nil
}

// Jobs returns a snapshot of every job.
func (m *Manager) Jobs() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, *j)
	}
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

// Fetch downloads a file, verifies it, and moves it into place.
//
// The returned Job is a snapshot of the final state; the file is only at
// FinalPath when State is StateComplete.
func (m *Manager) Fetch(ctx context.Context, job Job) (Job, error) {
	if job.ID == "" {
		job.ID = jobID(job.URL, job.Filename)
	}
	if job.Filename == "" {
		job.Filename = filenameFromURL(job.URL)
	}
	if job.DestDir == "" {
		return job, errors.New("download: no destination directory")
	}
	job.State = StatePending
	job.StartedAt = time.Now()

	m.mu.Lock()
	m.jobs[job.ID] = &job
	m.mu.Unlock()

	partial := filepath.Join(m.WorkDir, job.ID+".part")

	if err := m.transfer(ctx, job.ID, job.URL, partial); err != nil {
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

	m.update(job.ID, func(j *Job) { j.State = StateVerifying })

	sum, size, err := hashFile(partial)
	if err != nil {
		m.update(job.ID, func(j *Job) {
			j.State = StateFailed
			j.Error = err.Error()
			j.FinishedAt = time.Now()
		})
		final, _ := m.Job(job.ID)
		return final, err
	}

	m.update(job.ID, func(j *Job) {
		j.ActualSHA = sum
		j.Downloaded = size
	})

	// The check that keeps a corrupt or substituted file out of the index. A
	// mismatch leaves the partial in the work directory rather than deleting it:
	// the bytes may be worth inspecting, and quarantine means "not admitted",
	// not "destroyed".
	if job.ExpectedSHA256 != "" && !strings.EqualFold(sum, job.ExpectedSHA256) {
		m.update(job.ID, func(j *Job) {
			j.State = StateQuarantined
			j.Error = fmt.Sprintf("expected %s, got %s", job.ExpectedSHA256, sum)
			j.FinishedAt = time.Now()
		})
		final, _ := m.Job(job.ID)
		return final, fmt.Errorf("%w: %s", ErrChecksumMismatch, final.Error)
	}

	dest, err := m.publish(partial, job.DestDir, job.Filename)
	if err != nil {
		m.update(job.ID, func(j *Job) {
			j.State = StateFailed
			j.Error = err.Error()
			j.FinishedAt = time.Now()
		})
		final, _ := m.Job(job.ID)
		return final, err
	}

	m.update(job.ID, func(j *Job) {
		j.State = StateComplete
		j.FinalPath = dest
		j.FinishedAt = time.Now()
	})
	final, _ := m.Job(job.ID)
	return final, nil
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
		if m.APIKey != "" {
			// Civitai gates some models behind an account. Without this the
			// server returns an HTML login page with a 200, which would
			// otherwise be written to disk and named .safetensors.
			req.Header.Set("Authorization", "Bearer "+m.APIKey)
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
			// Already have the whole thing.
			resp.Body.Close()
			return nil
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			resp.Body.Close()
			return fmt.Errorf(
				"download: %s returned %d — this model may require a Civitai API key (--api-key)",
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
