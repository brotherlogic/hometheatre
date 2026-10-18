package nfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/brotherlogic/hometheatre/internal/metadata"
	"github.com/brotherlogic/hometheatre/internal/ripper"
	"golang.org/x/sys/unix"
)

const (
	// CopyBufferSize is the 1 MiB (1,048,576-byte) buffer size used when streaming .iso files to NFS.
	CopyBufferSize = 1024 * 1024

	StatusIdle                    = "Idle"
	StatusTransferringToNFS       = "Transferring to NFS"
	StatusNFSTransferPendingRetry = "NFS Transfer Pending (Retry)"
)

// ErrTargetAlreadyExists is returned when the destination .iso already exists on the NAS
// and overwrite is false.
var ErrTargetAlreadyExists = errors.New("target_already_exists")

// DefaultTransferRetryBackoffs defines the exponential backoff schedule (15s, 30s, 1m, 2m, capped at 5m)
// per DOCKER.md Section 4.4.
var DefaultTransferRetryBackoffs = []time.Duration{
	15 * time.Second,
	30 * time.Second,
	1 * time.Minute,
	2 * time.Minute,
	5 * time.Minute,
}

// WithNFSUser executes fn on a locked OS thread with its Linux filesystem
// credentials (fsuid/fsgid) set to puid:pgid so NFS RPCs authenticate as the
// configured NAS user even when root_squash is enabled on the NFS export.
func WithNFSUser(puid, pgid int, fn func() error) error {
	if puid <= 0 && pgid <= 0 {
		return fn()
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	unix.Setfsgid(pgid)
	unix.Setfsuid(puid)
	defer func() {
		unix.Setfsuid(0)
		unix.Setfsgid(0)
	}()

	return fn()
}

// TransferOutcome describes the final destination and metadata of a completed NFS transfer.
type TransferOutcome struct {
	JobID          string
	RawLabel       string
	Classification string
	AutoFiled      bool
	Reason         string
	FinalISOPath   string
	FinalNFOPath   string
	ProbeJSONPath  string
}

// TransferRetryEvent is emitted when a scratch -> NFS transfer fails transiently and enters retry backoff.
type TransferRetryEvent struct {
	JobID       string
	RawLabel    string
	Attempt     int
	NextBackoff time.Duration
	Err         error
}

// Writer handles atomic file staging, .nfo / .probe.json persistence, zero-copy renames,
// and exponential backoff retries on the NAS NFS share (/mnt/nfs).
type Writer struct {
	NFSRoot       string
	PUID          int
	PGID          int
	TMDB          *metadata.TMDBClient
	RunCommand    metadata.CommandRunner
	DVDCSSCache   string
	NFSUserRunner func(puid, pgid int, fn func() error) error
	CopyHook      func(dst io.Writer, src io.Reader, buf []byte) (int64, error)
	RetryBackoffs []time.Duration
	Now           func() time.Time
	Logger        *log.Logger

	OnTransferCompleted func(outcome TransferOutcome)
	OnTransferRetry     func(ev TransferRetryEvent)

	mu        sync.RWMutex
	status    string
	retryWake chan struct{}
}

// NewWriter creates an NFS Writer configured with production defaults.
func NewWriter(nfsRoot string, puid, pgid int, tmdb *metadata.TMDBClient) *Writer {
	return &Writer{
		NFSRoot:       nfsRoot,
		PUID:          puid,
		PGID:          pgid,
		TMDB:          tmdb,
		RunCommand:    ripper.DefaultCommandRunner,
		NFSUserRunner: WithNFSUser,
		RetryBackoffs: DefaultTransferRetryBackoffs,
		Now:           time.Now,
		Logger:        log.Default(),
		status:        StatusIdle,
		retryWake:     make(chan struct{}, 1),
	}
}

// Status returns the writer's live human-readable status ("Idle", "Transferring to NFS", or "NFS Transfer Pending (Retry)").
func (w *Writer) Status() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status
}

func (w *Writer) setStatus(s string) {
	w.mu.Lock()
	w.status = s
	w.mu.Unlock()
}

// TriggerRetry wakes the background transfer worker immediately if it is waiting in retry backoff.
func (w *Writer) TriggerRetry() {
	select {
	case w.retryWake <- struct{}{}:
	default:
	}
}

// ProcessJobWithRetry executes metadata re-probing (if needed), TMDB matching, and atomic NFS transfer,
// retrying with exponential backoff (15s, 30s, 1m, 2m, capped at 5m) on transient NFS errors until
// the transfer succeeds or ctx is canceled.
func (w *Writer) ProcessJobWithRetry(ctx context.Context, job *ripper.Job) (*TransferOutcome, error) {
	defer w.setStatus(StatusIdle)

	backoffs := w.RetryBackoffs
	if len(backoffs) == 0 {
		backoffs = DefaultTransferRetryBackoffs
	}

	for attempt := 1; ; attempt++ {
		w.setStatus(StatusTransferringToNFS)
		outcome, err := w.ProcessJobOnce(ctx, job)
		if err == nil {
			if w.OnTransferCompleted != nil {
				w.OnTransferCompleted(*outcome)
			}
			return outcome, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		idx := attempt - 1
		if idx >= len(backoffs) {
			idx = len(backoffs) - 1
		}
		waitDur := backoffs[idx]

		w.setStatus(StatusNFSTransferPendingRetry)
		w.logf("[%s] NFS transfer failed (attempt %d): %v; preserving %s and retrying in %s",
			job.JobID, attempt, err, job.ISOPath, waitDur)

		if w.OnTransferRetry != nil {
			w.OnTransferRetry(TransferRetryEvent{
				JobID:       job.JobID,
				RawLabel:    job.RawLabel,
				Attempt:     attempt,
				NextBackoff: waitDur,
				Err:         err,
			})
		}

		timer := time.NewTimer(waitDur)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-w.retryWake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// ProcessJobOnce executes a single attempt to probe/re-probe the job's local .iso, evaluate
// TMDB matching rules, and atomically transfer the .iso and companion .nfo / .probe.json to NFS.
func (w *Writer) ProcessJobOnce(ctx context.Context, job *ripper.Job) (*TransferOutcome, error) {
	if job == nil {
		return nil, errors.New("nil job")
	}

	// 1. Two-Point Probe Timing (METADATA.md Section 1.1):
	// Automatically re-probe the finalized local <job_dir>/disc.iso if the initial /dev/sr0
	// probe failed or returned zero valid tracks.
	if metadata.NeedsTrackReprobe(job.TracksSeconds) || metadata.NeedsLabelReprobe(job.RawLabel) {
		w.reprobeLocalISO(ctx, job)
	}

	// 2. Evaluate track classification, label sanitization, and TMDB matching (METADATA.md Sections 2 & 3)
	decision := metadata.EvaluateDisc(ctx, job.RawLabel, job.TracksSeconds, w.TMDB)

	// 3. Route to /movies if High-Confidence Movie Match (with fallback to /unidentified on target collision)
	if decision.AutoFileMovie && decision.MatchedMovie != nil {
		outcome, err := w.TransferMovie(job, decision.MatchedMovie, "", false)
		if err == nil {
			return outcome, nil
		}
		if errors.Is(err, ErrTargetAlreadyExists) {
			w.logf("[%s] Target movie ISO already exists on NFS; routing to /unidentified with reason=%s",
				job.JobID, metadata.ReasonTargetAlreadyExists)
			decision.AutoFileMovie = false
			decision.Reason = metadata.ReasonTargetAlreadyExists
		} else {
			return nil, err
		}
	}

	// 4. Route to /unidentified with .probe.json sidecar
	return w.TransferUnidentified(job, &decision)
}

func (w *Writer) reprobeLocalISO(ctx context.Context, job *ripper.Job) {
	runner := w.RunCommand
	if runner == nil {
		runner = ripper.DefaultCommandRunner
	}
	var env []string
	if w.DVDCSSCache != "" {
		env = []string{
			"DVDCSS_METHOD=key",
			"DVDCSS_CACHE=" + w.DVDCSSCache,
			"DVDCSS_VERBOSE=1",
		}
	} else if job.JobDir != "" {
		cacheDir := filepath.Join(filepath.Dir(job.JobDir), ".dvdcss")
		env = []string{
			"DVDCSS_METHOD=key",
			"DVDCSS_CACHE=" + cacheDir,
			"DVDCSS_VERBOSE=1",
		}
	}

	res, err := metadata.ProbeTarget(ctx, runner, env, job.ISOPath)
	if err != nil && res == nil {
		return
	}
	updated := false
	if metadata.NeedsLabelReprobe(job.RawLabel) && res.RawLabel != "" {
		job.RawLabel = res.RawLabel
		updated = true
	}
	if metadata.NeedsTrackReprobe(job.TracksSeconds) && len(res.TracksSeconds) > 0 {
		job.TracksSeconds = res.TracksSeconds
		updated = true
	}
	if updated && job.JobDir != "" {
		_ = ripper.WriteJobFile(job.JobDir, job)
	}
}

// TransferMovie atomically writes the companion Kodi .nfo and streams job.ISOPath to
// <NFS_ROOT>/movies/Movie Title (Year)/Movie Title (Year).iso inside WithNFSUser(puid, pgid).
// Deletes job.JobDir only after Sync(), Close(), and os.Rename() all succeed.
func (w *Writer) TransferMovie(job *ripper.Job, movie *metadata.TMDBMovieDetails, editionSuffix string, overwrite bool) (*TransferOutcome, error) {
	if job == nil || movie == nil {
		return nil, errors.New("job and movie must be non-nil")
	}

	nfoBytes, err := metadata.GenerateMovieNFO(movie, editionSuffix)
	if err != nil {
		return nil, fmt.Errorf("generate movie NFO: %w", err)
	}

	dirName := metadata.FormatMovieBaseName(movie.Title, movie.Year(), "")
	fileBase := metadata.FormatMovieBaseName(movie.Title, movie.Year(), editionSuffix)
	destDir := filepath.Join(w.NFSRoot, "movies", dirName)

	finalISOPath := filepath.Join(destDir, fileBase+".iso")
	partialISOPath := filepath.Join(destDir, "."+fileBase+".iso.partial")
	finalNFOPath := filepath.Join(destDir, fileBase+".nfo")
	partialNFOPath := filepath.Join(destDir, "."+fileBase+".nfo.partial")

	// Open local scratch ISO before switching thread fsuid/fsgid
	srcFile, err := os.Open(job.ISOPath)
	if err != nil {
		return nil, fmt.Errorf("open scratch ISO %s: %w", job.ISOPath, err)
	}
	defer srcFile.Close()

	err = w.withNFSUser(func() error {
		// 1. Create destination directory
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", destDir, err)
		}

		// 2. Check for target collision when overwrite is false
		if !overwrite {
			if _, statErr := os.Stat(finalISOPath); statErr == nil {
				return ErrTargetAlreadyExists
			} else if !os.IsNotExist(statErr) {
				return fmt.Errorf("stat %s: %w", finalISOPath, statErr)
			}
		}

		// 3. Write and fsync companion .nfo first
		if err := writeAtomicFile(partialNFOPath, finalNFOPath, nfoBytes); err != nil {
			return fmt.Errorf("write movie NFO %s: %w", finalNFOPath, err)
		}

		// 4, 5, 6. Stream .iso to hidden .partial file (1 MiB buffer), Sync(), Close(), and atomic os.Rename
		if err := w.streamAndRenameISO(srcFile, partialISOPath, finalISOPath); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	_ = srcFile.Close()
	// 7. Delete local scratch job directory only after full success
	if job.JobDir != "" {
		if err := os.RemoveAll(job.JobDir); err != nil {
			w.logf("Warning: failed to remove completed scratch dir %s: %v", job.JobDir, err)
		}
	}

	return &TransferOutcome{
		JobID:          job.JobID,
		RawLabel:       job.RawLabel,
		Classification: metadata.MediaTypeMovie,
		AutoFiled:      true,
		FinalISOPath:   finalISOPath,
		FinalNFOPath:   finalNFOPath,
	}, nil
}

// TransferUnidentified atomically writes <RAW_LABEL>_<TIMESTAMP>.probe.json and streams
// job.ISOPath to <NFS_ROOT>/unidentified/<RAW_LABEL>_<TIMESTAMP>.iso inside WithNFSUser(puid, pgid).
// Deletes job.JobDir only after Sync(), Close(), and os.Rename() all succeed.
func (w *Writer) TransferUnidentified(job *ripper.Job, decision *metadata.RoutingDecision) (*TransferOutcome, error) {
	if job == nil || decision == nil {
		return nil, errors.New("job and decision must be non-nil")
	}

	now := w.nowUTC()
	tsToken := extractJobTimestamp(job, now)
	safeLabel := ripper.SanitizeVolumeID(job.RawLabel)
	baseName := fmt.Sprintf("%s_%s", safeLabel, tsToken)

	isoFilename := baseName + ".iso"
	probeFilename := baseName + ".probe.json"

	destDir := filepath.Join(w.NFSRoot, "unidentified")
	finalISOPath := filepath.Join(destDir, isoFilename)
	partialISOPath := filepath.Join(destDir, "."+isoFilename+".partial")
	finalProbePath := filepath.Join(destDir, probeFilename)
	partialProbePath := filepath.Join(destDir, "."+probeFilename+".partial")

	rawLabel := strings.TrimSpace(job.RawLabel)
	if rawLabel == "" {
		rawLabel = safeLabel
	}

	probedAt := now.Format(time.RFC3339)
	if job.StartedAt != "" {
		probedAt = job.StartedAt
	}

	sidecar := &metadata.ProbeSidecar{
		ISOFilename:    isoFilename,
		ProbedAt:       probedAt,
		RawLabel:       rawLabel,
		SanitizedQuery: decision.SanitizedQuery,
		Classification: decision.Classification,
		Reason:         decision.Reason,
		Hints:          decision.Hints,
		TracksSeconds:  decision.TracksSeconds,
		TMDBCandidates: decision.Candidates,
	}
	probeBytes, err := metadata.MarshalProbeSidecar(sidecar)
	if err != nil {
		return nil, fmt.Errorf("marshal .probe.json: %w", err)
	}

	// Open local scratch ISO before switching thread fsuid/fsgid
	srcFile, err := os.Open(job.ISOPath)
	if err != nil {
		return nil, fmt.Errorf("open scratch ISO %s: %w", job.ISOPath, err)
	}
	defer srcFile.Close()

	err = w.withNFSUser(func() error {
		// 1. Create destination directory
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", destDir, err)
		}

		// 3. Write and fsync .probe.json first
		if err := writeAtomicFile(partialProbePath, finalProbePath, probeBytes); err != nil {
			return fmt.Errorf("write .probe.json %s: %w", finalProbePath, err)
		}

		// 4, 5, 6. Stream .iso to hidden .partial file (1 MiB buffer), Sync(), Close(), and atomic os.Rename
		if err := w.streamAndRenameISO(srcFile, partialISOPath, finalISOPath); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	_ = srcFile.Close()
	// 7. Delete local scratch job directory only after full success
	if job.JobDir != "" {
		if err := os.RemoveAll(job.JobDir); err != nil {
			w.logf("Warning: failed to remove completed scratch dir %s: %v", job.JobDir, err)
		}
	}

	return &TransferOutcome{
		JobID:          job.JobID,
		RawLabel:       rawLabel,
		Classification: decision.Classification,
		AutoFiled:      false,
		Reason:         decision.Reason,
		FinalISOPath:   finalISOPath,
		ProbeJSONPath:  finalProbePath,
	}, nil
}

// TagUnidentifiedAsMovie performs a zero-copy server-side os.Rename of an .iso in
// <NFS_ROOT>/unidentified/ into <NFS_ROOT>/movies/Movie Title (Year)/ alongside its .nfo,
// then removes the companion .probe.json sidecar (DOCKER.md Section 4.3).
func (w *Writer) TagUnidentifiedAsMovie(unidentifiedISOFilename string, movie *metadata.TMDBMovieDetails, editionSuffix string, overwrite bool) (string, error) {
	if movie == nil {
		return "", errors.New("nil movie details")
	}

	nfoBytes, err := metadata.GenerateMovieNFO(movie, editionSuffix)
	if err != nil {
		return "", fmt.Errorf("generate movie NFO: %w", err)
	}

	dirName := metadata.FormatMovieBaseName(movie.Title, movie.Year(), "")
	fileBase := metadata.FormatMovieBaseName(movie.Title, movie.Year(), editionSuffix)
	destDir := filepath.Join(w.NFSRoot, "movies", dirName)

	srcISOPath := filepath.Join(w.NFSRoot, "unidentified", filepath.Base(unidentifiedISOFilename))
	probePath := strings.TrimSuffix(srcISOPath, filepath.Ext(srcISOPath)) + ".probe.json"

	finalISOPath := filepath.Join(destDir, fileBase+".iso")
	finalNFOPath := filepath.Join(destDir, fileBase+".nfo")
	partialNFOPath := filepath.Join(destDir, "."+fileBase+".nfo.partial")

	err = w.withNFSUser(func() error {
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", destDir, err)
		}
		if !overwrite {
			if _, statErr := os.Stat(finalISOPath); statErr == nil {
				return ErrTargetAlreadyExists
			} else if !os.IsNotExist(statErr) {
				return fmt.Errorf("stat %s: %w", finalISOPath, statErr)
			}
		}
		if err := writeAtomicFile(partialNFOPath, finalNFOPath, nfoBytes); err != nil {
			return fmt.Errorf("write movie NFO %s: %w", finalNFOPath, err)
		}
		if err := os.Rename(srcISOPath, finalISOPath); err != nil {
			return fmt.Errorf("rename %s -> %s: %w", srcISOPath, finalISOPath, err)
		}
		_ = os.Remove(probePath)
		return nil
	})
	if err != nil {
		return "", err
	}
	return finalISOPath, nil
}

// TagUnidentifiedAsTVShow performs a zero-copy server-side os.Rename of an .iso in
// <NFS_ROOT>/unidentified/ into <NFS_ROOT>/tv/Show Title (Year)/Season XX/ alongside
// tvshow.nfo (created idempotently if missing) and the multi-episode .nfo, then removes .probe.json.
func (w *Writer) TagUnidentifiedAsTVShow(unidentifiedISOFilename string, show *metadata.TMDBTVDetails, season int, episodes []metadata.TMDBEpisode, overwrite bool) (string, error) {
	if show == nil {
		return "", errors.New("nil show details")
	}
	if len(episodes) == 0 {
		return "", errors.New("at least one episode is required")
	}

	tvshowNFOBytes, err := metadata.GenerateTVShowNFO(show)
	if err != nil {
		return "", fmt.Errorf("generate tvshow.nfo: %w", err)
	}
	episodeNFOBytes, err := metadata.GenerateMultiEpisodeNFO(show.Name, episodes)
	if err != nil {
		return "", fmt.Errorf("generate multi-episode NFO: %w", err)
	}

	var epNums []int
	for _, ep := range episodes {
		epNums = append(epNums, ep.EpisodeNumber)
	}

	showDirName := metadata.FormatShowDirName(show.Name, show.Year())
	seasonDirName := metadata.FormatSeasonDirName(season)
	fileBase := metadata.FormatMultiEpisodeBaseName(show.Name, season, epNums)

	showDir := filepath.Join(w.NFSRoot, "tv", showDirName)
	seasonDir := filepath.Join(showDir, seasonDirName)
	tvshowNFOPath := filepath.Join(showDir, "tvshow.nfo")
	partialTVShowNFOPath := filepath.Join(showDir, ".tvshow.nfo.partial")

	finalISOPath := filepath.Join(seasonDir, fileBase+".iso")
	finalNFOPath := filepath.Join(seasonDir, fileBase+".nfo")
	partialNFOPath := filepath.Join(seasonDir, "."+fileBase+".nfo.partial")

	srcISOPath := filepath.Join(w.NFSRoot, "unidentified", filepath.Base(unidentifiedISOFilename))
	probePath := strings.TrimSuffix(srcISOPath, filepath.Ext(srcISOPath)) + ".probe.json"

	err = w.withNFSUser(func() error {
		if err := os.MkdirAll(seasonDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", seasonDir, err)
		}
		if !overwrite {
			if _, statErr := os.Stat(finalISOPath); statErr == nil {
				return ErrTargetAlreadyExists
			} else if !os.IsNotExist(statErr) {
				return fmt.Errorf("stat %s: %w", finalISOPath, statErr)
			}
		}

		// Idempotently write tvshow.nfo if it does not already exist
		if _, statErr := os.Stat(tvshowNFOPath); os.IsNotExist(statErr) {
			if err := writeAtomicFile(partialTVShowNFOPath, tvshowNFOPath, tvshowNFOBytes); err != nil {
				return fmt.Errorf("write tvshow.nfo: %w", err)
			}
		}

		if err := writeAtomicFile(partialNFOPath, finalNFOPath, episodeNFOBytes); err != nil {
			return fmt.Errorf("write multi-episode NFO %s: %w", finalNFOPath, err)
		}
		if err := os.Rename(srcISOPath, finalISOPath); err != nil {
			return fmt.Errorf("rename %s -> %s: %w", srcISOPath, finalISOPath, err)
		}
		_ = os.Remove(probePath)
		return nil
	})
	if err != nil {
		return "", err
	}
	return finalISOPath, nil
}

func (w *Writer) streamAndRenameISO(srcFile *os.File, partialISOPath, finalISOPath string) error {
	if _, err := srcFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek scratch ISO: %w", err)
	}

	dstFile, err := os.OpenFile(partialISOPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open partial ISO %s: %w", partialISOPath, err)
	}

	buf := make([]byte, CopyBufferSize)
	copyFn := w.CopyHook
	if copyFn == nil {
		copyFn = io.CopyBuffer
	}
	if _, err := copyFn(dstFile, srcFile, buf); err != nil {
		_ = dstFile.Close()
		_ = os.Remove(partialISOPath)
		return fmt.Errorf("copy ISO to %s: %w", partialISOPath, err)
	}

	if err := dstFile.Sync(); err != nil {
		_ = dstFile.Close()
		_ = os.Remove(partialISOPath)
		return fmt.Errorf("fsync partial ISO %s: %w", partialISOPath, err)
	}

	if err := dstFile.Close(); err != nil {
		_ = os.Remove(partialISOPath)
		return fmt.Errorf("close partial ISO %s: %w", partialISOPath, err)
	}

	if err := os.Rename(partialISOPath, finalISOPath); err != nil {
		_ = os.Remove(partialISOPath)
		return fmt.Errorf("rename %s -> %s: %w", partialISOPath, finalISOPath, err)
	}

	return nil
}

func writeAtomicFile(partialPath, finalPath string, data []byte) error {
	f, err := os.OpenFile(partialPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(partialPath)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(partialPath)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(partialPath)
		return err
	}
	if err := os.Rename(partialPath, finalPath); err != nil {
		_ = os.Remove(partialPath)
		return err
	}
	return nil
}

func (w *Writer) withNFSUser(fn func() error) error {
	runner := w.NFSUserRunner
	if runner == nil {
		runner = WithNFSUser
	}
	return runner(w.PUID, w.PGID, fn)
}

func extractJobTimestamp(job *ripper.Job, fallback time.Time) string {
	if job != nil {
		if strings.HasPrefix(job.JobID, "job_") && len(job.JobID) > 4 {
			return strings.TrimPrefix(job.JobID, "job_")
		}
		if t, err := time.Parse(time.RFC3339, job.StartedAt); err == nil {
			return t.UTC().Format("20060102T150405Z")
		}
	}
	return fallback.UTC().Format("20060102T150405Z")
}

func (w *Writer) nowUTC() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *Writer) logf(format string, args ...any) {
	if w.Logger != nil {
		w.Logger.Printf(format, args...)
	}
}
