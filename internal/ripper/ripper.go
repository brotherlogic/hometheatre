package ripper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/brotherlogic/hometheatre/internal/drive"
	"github.com/brotherlogic/hometheatre/internal/metadata"
	"golang.org/x/sys/unix"
)

const (
	// MinScratchFreeBytes is the pre-rip space gate threshold (18 GiB).
	MinScratchFreeBytes uint64 = 18 * 1024 * 1024 * 1024

	// MinISOSizeBytes is the minimum valid DVD ISO size (500 MiB = 524,288,000 bytes).
	MinISOSizeBytes int64 = 500 * 1024 * 1024

	// MinRescuedRatio is the minimum fraction of rescued bytes required in ddrescue.map (95.0%).
	MinRescuedRatio float64 = 0.95

	// DefaultVolumeLabel is the fallback volume ID when blkid returns an empty or invalid label.
	DefaultVolumeLabel = "DVD_VIDEO"

	// MaxVolumeLabelLen is the maximum volume ID length permitted by genisoimage -V.
	MaxVolumeLabelLen = 32

	// Job states persisted in <job_dir>/job.json
	StateRipping      = "ripping"
	StateISOFinalized = "iso_finalized"

	// Ripping pipeline stages persisted in <job_dir>/job.json
	Stage1DVDBackup   = "stage1_dvdbackup"
	Stage1GenISOImage = "stage1_genisoimage"
	Stage2DDRescue    = "stage2_ddrescue"

	// Worker live status strings for Web UI reporting
	StatusIdle                   = "Idle"
	StatusWaitingForScratch      = "Waiting for Scratch Space"
	StatusRippingStage1DVDBackup = "Ripping (stage1_dvdbackup)"
	StatusRippingStage1GenISO    = "Ripping (stage1_genisoimage)"
	StatusRippingStage2DDRescue  = "Ripping (stage2_ddrescue)"

	// DefaultSpaceWaitInterval is the polling interval while waiting for an active NFS transfer to free scratch space.
	DefaultSpaceWaitInterval = 2 * time.Second
)

// DefaultStage1RetryBackoffs defines the retry backoff schedule between Stage 1
// dvdbackup attempts (up to 3 total attempts: initial + 2 retries with 2s, 3s backoff).
var DefaultStage1RetryBackoffs = []time.Duration{
	2 * time.Second,
	3 * time.Second,
}

// DefaultEjectRetryBackoffs defines the retry backoff schedule when ejecting
// the optical drive fails or CDROM_DRIVE_STATUS remains CDS_DISC_OK / CDS_DRIVE_NOT_READY
// (up to 3 total attempts: initial + 2 retries with 1s, 2s backoff).
var DefaultEjectRetryBackoffs = []time.Duration{
	1 * time.Second,
	2 * time.Second,
}

// ErrInsufficientScratchSpace is returned when available scratch disk space is < 18 GiB
// and no in-flight or queued NFS transfer is freeing space.
var ErrInsufficientScratchSpace = errors.New("insufficient_scratch_space")

// Job represents the persistent job state written to <job_dir>/job.json.
type Job struct {
	JobID         string    `json:"job_id"`
	StartedAt     string    `json:"started_at"`
	State         string    `json:"state"`
	Stage         string    `json:"stage"`
	RawLabel      string    `json:"raw_label"`
	TracksSeconds []float64 `json:"tracks_seconds"`
	RescuedRatio  float64   `json:"rescued_ratio,omitempty"`

	JobDir  string `json:"-"`
	ISOPath string `json:"-"`
}

// Progress represents live byte and percentage progress for an active rip job.
type Progress struct {
	Active     bool    `json:"active"`
	Stage      string  `json:"stage,omitempty"`
	Label      string  `json:"label,omitempty"`
	BytesDone  int64   `json:"bytes_done"`
	BytesTotal int64   `json:"bytes_total"`
	Percent    float64 `json:"percent"`
}

// FailureEvent captures diagnostic metadata when a rip aborts or fails validation.
type FailureEvent struct {
	JobID        string
	RawLabel     string
	Stage        string
	Reason       string
	RescuedRatio float64
	StderrTail   string
	Err          error
}

// CommandRunner executes an external CLI binary with the provided environment variables and arguments.
type CommandRunner func(ctx context.Context, env []string, name string, args ...string) (output string, err error)

// StatfsFunc returns the available free bytes on the filesystem containing dir.
type StatfsFunc func(dir string) (uint64, error)

// DiscSizeFunc returns the byte capacity of the optical disc in devicePath.
type DiscSizeFunc func(devicePath string) (int64, error)

// PendingTransfersFunc returns true if an in-flight or queued NFS transfer is currently
// holding a finalized .iso in the scratch directory.
type PendingTransfersFunc func() bool

// JobFinalizedHandler is called when a rip produces a validated local .iso and ejects the disc.
type JobFinalizedHandler func(job *Job)

// RipFailedHandler is called when a rip fails (e.g. insufficient space or Stage 2 failure).
type RipFailedHandler func(event FailureEvent)

// DefaultCommandRunner executes commands via os/exec with merged process environment variables.
func DefaultCommandRunner(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// CheckScratchSpace queries unix.Statfs on scratchDir and returns available bytes (Bavail * Bsize).
func CheckScratchSpace(scratchDir string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(scratchDir, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", scratchDir, err)
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

// QueryDiscCapacity queries the total byte capacity of the optical disc on devicePath
// via ioctl(BLKGETSIZE64), falling back to /sys/class/block/<dev>/size (512-byte sectors)
// or regular file size.
func QueryDiscCapacity(devicePath string) (int64, error) {
	if fd, err := unix.Open(devicePath, unix.O_RDONLY|unix.O_NONBLOCK, 0); err == nil {
		var size uint64
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.BLKGETSIZE64), uintptr(unsafe.Pointer(&size)))
		_ = unix.Close(fd)
		if errno == 0 && size > 0 {
			return int64(size), nil
		}
	}

	if size, err := readSysfsBlockCapacity("/sys/class/block", devicePath); err == nil && size > 0 {
		return size, nil
	}

	if info, err := os.Stat(devicePath); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return info.Size(), nil
	}

	return 0, fmt.Errorf("unable to query disc capacity for %s", devicePath)
}

func readSysfsBlockCapacity(sysfsBlockDir, devicePath string) (int64, error) {
	devBase := filepath.Base(strings.TrimSpace(devicePath))
	if devBase == "" || devBase == "." || devBase == "/" {
		return 0, fmt.Errorf("invalid device path %q", devicePath)
	}
	sizePath := filepath.Join(sysfsBlockDir, devBase, "size")
	data, err := os.ReadFile(sizePath)
	if err != nil {
		return 0, err
	}
	sectors, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, err
	}
	// Linux sysfs /sys/class/block/<dev>/size always reports 512-byte sectors.
	return int64(sectors) * 512, nil
}

// SanitizeVolumeID sanitizes a raw blkid DVD label to [A-Z0-9_] and truncates it to
// at most 32 characters for genisoimage -V, falling back to "DVD_VIDEO" if empty.
func SanitizeVolumeID(raw string) string {
	upper := strings.ToUpper(strings.TrimSpace(raw))
	var b strings.Builder
	b.Grow(len(upper))

	lastUnderscore := false
	hasAlnum := false
	for _, r := range upper {
		switch {
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastUnderscore = false
			hasAlnum = true
		case r == '_' || r == ' ' || r == '-' || r == '.':
			if b.Len() > 0 && !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}

	cleaned := strings.Trim(b.String(), "_")
	if cleaned == "" || !hasAlnum {
		return DefaultVolumeLabel
	}
	if len(cleaned) > MaxVolumeLabelLen {
		cleaned = strings.TrimRight(cleaned[:MaxVolumeLabelLen], "_")
		if cleaned == "" {
			return DefaultVolumeLabel
		}
	}
	return cleaned
}

// ParseDDRescueMap parses a GNU ddrescue mapfile and calculates rescued bytes, total bytes,
// and the rescued ratio (rescued_bytes / total_bytes).
//
// Every non-comment block line after the status line in ddrescue.map has the format:
//
//	<pos_hex>  <size_hex>  <status_char>
//
// where <status_char> is '+' for successfully rescued blocks and '?', '*', '/', or '-'
// for non-tried/non-trimmed/non-scraped/bad-sector blocks.
func ParseDDRescueMap(r io.Reader) (rescuedBytes uint64, totalBytes uint64, ratio float64, err error) {
	scanner := bufio.NewScanner(r)
	sawStatusLine := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip inline comments if any
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		// The first non-comment line in a GNU ddrescue mapfile is the current status line:
		//   <current_pos_hex> <current_status_char> [current_pass]
		// Detect and skip it if fields[1] is a single status character rather than a block size.
		if !sawStatusLine && len(fields[1]) == 1 && isDDRescueStatusChar(fields[1][0]) {
			sawStatusLine = true
			continue
		}
		sawStatusLine = true

		if len(fields) < 3 || len(fields[2]) != 1 || !isDDRescueStatusChar(fields[2][0]) {
			return 0, 0, 0, fmt.Errorf("invalid ddrescue mapfile block line: %q", line)
		}

		if _, err := strconv.ParseUint(fields[0], 0, 64); err != nil {
			return 0, 0, 0, fmt.Errorf("invalid block position %q: %w", fields[0], err)
		}
		size, err := strconv.ParseUint(fields[1], 0, 64)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("invalid block size %q: %w", fields[1], err)
		}

		statusChar := fields[2][0]
		totalBytes += size
		if statusChar == '+' {
			rescuedBytes += size
		}
	}

	if err := scanner.Err(); err != nil {
		return 0, 0, 0, err
	}
	if totalBytes == 0 {
		return 0, 0, 0, errors.New("ddrescue mapfile contains zero total bytes")
	}

	ratio = float64(rescuedBytes) / float64(totalBytes)
	return rescuedBytes, totalBytes, ratio, nil
}

func isDDRescueStatusChar(c byte) bool {
	switch c {
	case '+', '?', '*', '/', '-':
		return true
	default:
		return false
	}
}

// WriteJobFile atomically writes and fsyncs <jobDir>/job.json.
func WriteJobFile(jobDir string, job *Job) error {
	if job.TracksSeconds == nil {
		job.TracksSeconds = []float64{}
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal job.json: %w", err)
	}
	data = append(data, '\n')

	tmpPath := filepath.Join(jobDir, "job.json.tmp")
	finalPath := filepath.Join(jobDir, "job.json")

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", tmpPath, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("fsync %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, finalPath, err)
	}
	return nil
}

// ReadJobFile reads and parses <jobDir>/job.json.
func ReadJobFile(jobDir string) (*Job, error) {
	data, err := os.ReadFile(filepath.Join(jobDir, "job.json"))
	if err != nil {
		return nil, err
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, err
	}
	job.JobDir = jobDir
	job.ISOPath = filepath.Join(jobDir, "disc.iso")
	return &job, nil
}

// ReconcileScratch scans <scratchDir>/job_* on daemon startup:
//   - Interrupted rips (state != "iso_finalized" or missing/invalid disc.iso < 500 MiB)
//     are deleted immediately to reclaim scratch space.
//   - Finalized rips (state == "iso_finalized" with disc.iso >= 500 MiB) are preserved
//     and passed to enqueue (if non-nil) for background TMDB + NFS transfer.
func ReconcileScratch(scratchDir string, enqueue JobFinalizedHandler) ([]*Job, error) {
	entries, err := os.ReadDir(scratchDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("readdir %s: %w", scratchDir, err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "job_") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	var recovered []*Job
	for _, name := range names {
		jobDir := filepath.Join(scratchDir, name)
		job, err := ReadJobFile(jobDir)
		if err != nil || job.State != StateISOFinalized {
			_ = os.RemoveAll(jobDir)
			continue
		}

		info, err := os.Stat(job.ISOPath)
		if err != nil || info.IsDir() || info.Size() < MinISOSizeBytes {
			_ = os.RemoveAll(jobDir)
			continue
		}

		recovered = append(recovered, job)
		if enqueue != nil {
			enqueue(job)
		}
	}
	return recovered, nil
}

// Worker orchestrates the pre-rip scratch space gate, two-stage DVD ISO ripping
// pipeline (Stage 1 dvdbackup + genisoimage -> Stage 2 ddrescue fallback),
// immediate physical disc ejection, and handoff to the background transfer queue.
type Worker struct {
	ScratchDir          string
	DevicePath          string
	MinFreeBytes        uint64
	MinISOSize          int64
	SpaceWaitInterval   time.Duration
	Stage1RetryBackoffs []time.Duration
	EjectRetryBackoffs  []time.Duration
	RunCommand          CommandRunner
	CheckSpace          StatfsFunc
	QueryDiscSize       DiscSizeFunc
	QueryDriveStatus    drive.StatusQueryFunc
	Sleep               func(ctx context.Context, d time.Duration) error
	HasPendingTransfers PendingTransfersFunc
	OnJobFinalized      JobFinalizedHandler
	OnRipFailed         RipFailedHandler
	Now                 func() time.Time
	Logger              *log.Logger

	mu               sync.RWMutex
	status           string
	activeJobDir     string
	activeStage      string
	activeLabel      string
	discTotalBytes   int64
	stage1TotalBytes int64
}

// NewWorker constructs a Worker with production defaults for /dev/sr0 and /scratch.
func NewWorker(scratchDir, devicePath string) *Worker {
	return &Worker{
		ScratchDir:          scratchDir,
		DevicePath:          devicePath,
		MinFreeBytes:        MinScratchFreeBytes,
		MinISOSize:          MinISOSizeBytes,
		SpaceWaitInterval:   DefaultSpaceWaitInterval,
		Stage1RetryBackoffs: DefaultStage1RetryBackoffs,
		EjectRetryBackoffs:  DefaultEjectRetryBackoffs,
		RunCommand:          DefaultCommandRunner,
		CheckSpace:          CheckScratchSpace,
		QueryDiscSize:       QueryDiscCapacity,
		QueryDriveStatus:    drive.QueryDriveStatus,
		Now:                 time.Now,
		Logger:              log.Default(),
		status:              StatusIdle,
	}
}

// Status returns the worker's current human-readable status for the Web UI.
func (w *Worker) Status() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status
}

func (w *Worker) setStatus(status string) {
	w.mu.Lock()
	w.status = status
	w.mu.Unlock()
}

func (w *Worker) setActiveStage(status, stage string, stage1Total int64) {
	w.mu.Lock()
	w.status = status
	w.activeStage = stage
	if stage1Total > 0 {
		w.stage1TotalBytes = stage1Total
	}
	w.mu.Unlock()
}

func (w *Worker) resetRipState() {
	w.mu.Lock()
	w.status = StatusIdle
	w.activeJobDir = ""
	w.activeStage = ""
	w.activeLabel = ""
	w.discTotalBytes = 0
	w.stage1TotalBytes = 0
	w.mu.Unlock()
}

// Progress computes live byte and percentage progress for the currently active rip job.
func (w *Worker) Progress() Progress {
	w.mu.RLock()
	status := w.status
	jobDir := w.activeJobDir
	stage := w.activeStage
	label := w.activeLabel
	discTotal := w.discTotalBytes
	stage1Total := w.stage1TotalBytes
	w.mu.RUnlock()

	if stage == "" {
		switch status {
		case StatusRippingStage1DVDBackup:
			stage = Stage1DVDBackup
		case StatusRippingStage1GenISO:
			stage = Stage1GenISOImage
		case StatusRippingStage2DDRescue:
			stage = Stage2DDRescue
		}
	}
	if stage == "" {
		return Progress{Active: false}
	}

	var bytesDone, bytesTotal int64
	switch stage {
	case Stage1DVDBackup:
		if jobDir != "" {
			videoTSDir := filepath.Join(jobDir, "dvd", "VIDEO_TS")
			bytesDone = directorySize(videoTSDir)
			if bytesDone == 0 {
				bytesDone = directorySize(filepath.Join(jobDir, "dvd"))
			}
		}
		bytesTotal = discTotal

	case Stage1GenISOImage:
		if jobDir != "" {
			bytesDone = regularFileSize(filepath.Join(jobDir, "disc.iso"))
			if dvdBytes := directorySize(filepath.Join(jobDir, "dvd")); dvdBytes > 0 {
				bytesTotal = dvdBytes
			}
		}
		if bytesTotal <= 0 && stage1Total > 0 {
			bytesTotal = stage1Total
		}
		if bytesTotal <= 0 {
			bytesTotal = discTotal
		}

	case Stage2DDRescue:
		bytesTotal = discTotal
		if jobDir != "" {
			mapPath := filepath.Join(jobDir, "ddrescue.map")
			if f, err := os.Open(mapPath); err == nil {
				rescued, mapTotal, _, parseErr := ParseDDRescueMap(f)
				_ = f.Close()
				if parseErr == nil {
					bytesDone = int64(rescued)
					if bytesTotal <= 0 && mapTotal > 0 {
						bytesTotal = int64(mapTotal)
					}
				}
			}
			if bytesDone == 0 {
				bytesDone = regularFileSize(filepath.Join(jobDir, "disc.iso"))
			}
		}
	}

	var pct float64
	if bytesTotal > 0 && bytesDone > 0 {
		pct = (float64(bytesDone) / float64(bytesTotal)) * 100.0
		if pct > 100.0 {
			pct = 100.0
		}
		if pct < 0.0 {
			pct = 0.0
		}
	}

	return Progress{
		Active:     true,
		Stage:      stage,
		Label:      label,
		BytesDone:  bytesDone,
		BytesTotal: bytesTotal,
		Percent:    pct,
	}
}

func directorySize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, infoErr := d.Info(); infoErr == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func regularFileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return 0
	}
	return info.Size()
}

func (w *Worker) queryDiscSize(devicePath string) int64 {
	fn := w.QueryDiscSize
	if fn == nil {
		fn = QueryDiscCapacity
	}
	size, err := fn(devicePath)
	if err != nil || size < 0 {
		return 0
	}
	return size
}

// RipDisc executes the full pre-rip capacity gate and two-stage DVD ISO ripping pipeline.
func (w *Worker) RipDisc(ctx context.Context, devicePath string) (*Job, error) {
	if devicePath == "" {
		devicePath = w.DevicePath
	}
	defer w.resetRipState()

	if err := os.MkdirAll(w.ScratchDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir scratch %s: %w", w.ScratchDir, err)
	}
	dvdcssCacheDir := filepath.Join(w.ScratchDir, ".dvdcss")
	if err := os.MkdirAll(dvdcssCacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir dvdcss cache %s: %w", dvdcssCacheDir, err)
	}

	// 1. Pre-Rip Scratch Space Gate (>= 18 GiB)
	if err := w.waitForScratchSpace(ctx, devicePath); err != nil {
		return nil, err
	}

	// Query optical disc total byte capacity for live Web UI progress reporting
	discTotalBytes := w.queryDiscSize(devicePath)

	// 2. Probe raw volume label (blkid primary, lsdvd -Ox <title> fallback) and track durations (lsdvd -Ox)
	probeRes, probeErr := metadata.ProbeTarget(ctx, w.runCmd, w.dvdcssEnv(), devicePath)
	rawLabel := ""
	tracksSeconds := []float64{}
	if probeRes != nil {
		rawLabel = probeRes.RawLabel
		if len(probeRes.TracksSeconds) > 0 {
			tracksSeconds = probeRes.TracksSeconds
		}
	}
	safeLabel := SanitizeVolumeID(rawLabel)
	displayLabel := strings.TrimSpace(rawLabel)
	if displayLabel == "" {
		displayLabel = safeLabel
	}

	now := w.nowUTC()
	jobID := fmt.Sprintf("job_%s", now.Format("20060102T150405Z"))
	jobDir := filepath.Join(w.ScratchDir, jobID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir job dir %s: %w", jobDir, err)
	}

	w.mu.Lock()
	w.activeJobDir = jobDir
	w.activeLabel = displayLabel
	w.discTotalBytes = discTotalBytes
	w.stage1TotalBytes = 0
	w.mu.Unlock()

	job := &Job{
		JobID:         jobID,
		StartedAt:     now.Format(time.RFC3339),
		State:         StateRipping,
		Stage:         Stage1DVDBackup,
		RawLabel:      rawLabel,
		TracksSeconds: tracksSeconds,
		JobDir:        jobDir,
		ISOPath:       filepath.Join(jobDir, "disc.iso"),
	}
	if err := WriteJobFile(jobDir, job); err != nil {
		_ = os.RemoveAll(jobDir)
		return nil, err
	}

	if probeErr != nil {
		w.logf("[%s] Initial probe on %s: label=%q (safe=%q), tracks=%d, err=%v",
			job.JobID, devicePath, rawLabel, safeLabel, len(tracksSeconds), probeErr)
	} else {
		w.logf("[%s] Initial probe on %s: label=%q (safe=%q), tracks=%d, err=nil",
			job.JobID, devicePath, rawLabel, safeLabel, len(tracksSeconds))
	}

	// 3. Stage 1: dvdbackup + genisoimage
	if err := w.runStage1(ctx, devicePath, safeLabel, job); err == nil {
		// Validate finalized ISO size >= 500 MiB
		if w.validateISOSize(job.ISOPath) {
			return w.finalizeAndEject(ctx, devicePath, job)
		}
		w.logf("[%s] Stage 1 disc.iso failed minimum size validation; falling back to Stage 2 (ddrescue)", job.JobID)
	} else {
		w.logf("[%s] Stage 1 failed (%v); falling back to Stage 2 (ddrescue)", job.JobID, err)
	}

	// Clean up any partial Stage 1 artifacts before starting Stage 2
	_ = os.RemoveAll(filepath.Join(jobDir, "dvd"))
	_ = os.Remove(job.ISOPath)

	// 4. Stage 2 Fallback: CSS Bus Re-Unlock + ddrescue
	rescuedRatio, stderrTail, err := w.runStage2(ctx, devicePath, job)
	if err != nil {
		w.logf("[%s] Stage 2 failed: %v", job.JobID, err)
		_ = os.RemoveAll(jobDir)
		w.ejectBestEffort(ctx, devicePath)
		w.notifyFailure(FailureEvent{
			JobID:        job.JobID,
			RawLabel:     job.RawLabel,
			Stage:        Stage2DDRescue,
			Reason:       "stage2_ddrescue_failed",
			RescuedRatio: rescuedRatio,
			StderrTail:   stderrTail,
			Err:          err,
		})
		return nil, err
	}

	job.RescuedRatio = rescuedRatio
	return w.finalizeAndEject(ctx, devicePath, job)
}

func (w *Worker) waitForScratchSpace(ctx context.Context, devicePath string) error {
	minFree := w.MinFreeBytes
	if minFree == 0 {
		minFree = MinScratchFreeBytes
	}
	checkFn := w.CheckSpace
	if checkFn == nil {
		checkFn = CheckScratchSpace
	}
	waitInterval := w.SpaceWaitInterval
	if waitInterval <= 0 {
		waitInterval = DefaultSpaceWaitInterval
	}

	for {
		avail, err := checkFn(w.ScratchDir)
		if err != nil {
			return err
		}
		if avail >= minFree {
			return nil
		}

		if !w.hasActiveOrQueuedTransfers() {
			w.logf("Insufficient scratch space on %s (%d bytes available, need >= %d bytes) and no pending transfers; aborting rip",
				w.ScratchDir, avail, minFree)
			w.ejectBestEffort(ctx, devicePath)
			w.notifyFailure(FailureEvent{
				Stage:  "pre_rip_space_gate",
				Reason: ErrInsufficientScratchSpace.Error(),
				Err:    ErrInsufficientScratchSpace,
			})
			return ErrInsufficientScratchSpace
		}

		w.setStatus(StatusWaitingForScratch)
		w.logf("Scratch space low (%d bytes < %d bytes); waiting for active NFS transfer to free scratch space...",
			avail, minFree)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitInterval):
		}
	}
}

func (w *Worker) hasActiveOrQueuedTransfers() bool {
	if w.HasPendingTransfers != nil && w.HasPendingTransfers() {
		return true
	}
	// Also check if any finalized job_* directory currently exists on scratchDir.
	entries, err := os.ReadDir(w.ScratchDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "job_") {
			continue
		}
		job, err := ReadJobFile(filepath.Join(w.ScratchDir, entry.Name()))
		if err == nil && job.State == StateISOFinalized {
			return true
		}
	}
	return false
}

func (w *Worker) runStage1(ctx context.Context, devicePath, safeLabel string, job *Job) error {
	// Step 1A: dvdbackup -M -r a -n dvd -i /dev/sr0 -o <job_dir>
	job.Stage = Stage1DVDBackup
	_ = WriteJobFile(job.JobDir, job)
	w.setActiveStage(StatusRippingStage1DVDBackup, Stage1DVDBackup, 0)

	env := w.dvdcssEnv()
	dvdDir := filepath.Join(job.JobDir, "dvd")
	backoffs := w.stage1Backoffs()
	totalAttempts := len(backoffs) + 1

	var lastErr error
	for attempt := 1; attempt <= totalAttempts; attempt++ {
		if attempt > 1 {
			backoff := backoffs[attempt-2]
			w.logf("[%s] Stage 1 dvdbackup attempt %d/%d failed (%v); backing off %s and verifying drive readiness before retry",
				job.JobID, attempt-1, totalAttempts, lastErr, backoff)
			if err := w.sleep(ctx, backoff); err != nil {
				return err
			}
			if _, readyErr := drive.WaitForDiscReady(ctx, devicePath, w.queryDriveStatusFn(), backoff, backoff/4); readyErr != nil {
				w.logf("[%s] Stage 1 dvdbackup retry %d/%d skipped: drive %s not ready (%v)",
					job.JobID, attempt, totalAttempts, devicePath, readyErr)
				lastErr = fmt.Errorf("dvdbackup retry %d/%d drive not ready (%w); last error: %v", attempt, totalAttempts, readyErr, lastErr)
				continue
			}
			w.logf("[%s] Drive %s ready (CDS_DISC_OK); retrying Stage 1 dvdbackup (attempt %d/%d)",
				job.JobID, devicePath, attempt, totalAttempts)
		}

		out, err := w.runCmd(ctx, env, "dvdbackup", "-M", "-r", "a", "-n", "dvd", "-i", devicePath, "-o", job.JobDir)
		if err == nil {
			lastErr = nil
			break
		}
		_ = os.RemoveAll(dvdDir)
		lastErr = fmt.Errorf("dvdbackup failed: %w (output: %s)", err, tailString(out, 512))
	}
	if lastErr != nil {
		return lastErr
	}

	// Step 1B: genisoimage -quiet -dvd-video -V <SAFE_LABEL_32> -o <job_dir>/disc.iso <job_dir>/dvd
	job.Stage = Stage1GenISOImage
	_ = WriteJobFile(job.JobDir, job)
	w.setActiveStage(StatusRippingStage1GenISO, Stage1GenISOImage, directorySize(dvdDir))

	out, err := w.runCmd(ctx, env, "genisoimage", "-quiet", "-dvd-video", "-V", safeLabel, "-o", job.ISOPath, dvdDir)
	// Always delete <job_dir>/dvd once genisoimage finishes
	_ = os.RemoveAll(dvdDir)
	if err != nil {
		_ = os.Remove(job.ISOPath)
		return fmt.Errorf("genisoimage failed: %w (output: %s)", err, tailString(out, 512))
	}

	return nil
}

func (w *Worker) stage1Backoffs() []time.Duration {
	if w.Stage1RetryBackoffs != nil {
		return w.Stage1RetryBackoffs
	}
	return DefaultStage1RetryBackoffs
}

func (w *Worker) queryDriveStatusFn() drive.StatusQueryFunc {
	if w.QueryDriveStatus != nil {
		return w.QueryDriveStatus
	}
	return drive.QueryDriveStatus
}

func (w *Worker) sleep(ctx context.Context, d time.Duration) error {
	if w.Sleep != nil {
		return w.Sleep(ctx, d)
	}
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (w *Worker) runStage2(ctx context.Context, devicePath string, job *Job) (float64, string, error) {
	job.Stage = Stage2DDRescue
	_ = WriteJobFile(job.JobDir, job)
	w.setActiveStage(StatusRippingStage2DDRescue, Stage2DDRescue, 0)

	env := w.dvdcssEnv()

	// Step 2A: Re-unlock optical drive CSS bus:
	// lsdvd -x /dev/sr0 >/dev/null 2>&1 || dvdbackup -I -i /dev/sr0 >/dev/null 2>&1
	if _, err := w.runCmd(ctx, env, "lsdvd", "-x", devicePath); err != nil {
		_, _ = w.runCmd(ctx, env, "dvdbackup", "-I", "-i", devicePath)
	}

	// Step 2B: Fast-skip ddrescue invocation:
	// ddrescue -b 2048 -n -N -K 64Ki,1Mi -O -T 2m /dev/sr0 <job_dir>/disc.iso <job_dir>/ddrescue.map
	mapPath := filepath.Join(job.JobDir, "ddrescue.map")
	_ = os.Remove(mapPath)
	out, ddErr := w.runCmd(
		ctx,
		env,
		"ddrescue",
		"-b", "2048",
		"-n",
		"-N",
		"-K", "64Ki,1Mi",
		"-O",
		"-T", "2m",
		devicePath,
		job.ISOPath,
		mapPath,
	)
	stderrTail := tailString(out, 1024)

	// Parse ddrescue.map if present so we can report rescuedRatio even on failure
	var rescuedRatio float64
	if mapFile, err := os.Open(mapPath); err == nil {
		_, _, ratio, parseErr := ParseDDRescueMap(mapFile)
		_ = mapFile.Close()
		if parseErr == nil {
			rescuedRatio = ratio
		} else if ddErr == nil {
			ddErr = fmt.Errorf("parse ddrescue.map: %w", parseErr)
		}
	} else if ddErr == nil {
		ddErr = fmt.Errorf("open ddrescue.map: %w", err)
	}

	if ddErr != nil {
		return rescuedRatio, stderrTail, fmt.Errorf("ddrescue failed: %w", ddErr)
	}

	if !w.validateISOSize(job.ISOPath) {
		return rescuedRatio, stderrTail, fmt.Errorf("ddrescue output disc.iso is smaller than minimum %d bytes", w.minISOBytes())
	}

	if rescuedRatio < MinRescuedRatio {
		return rescuedRatio, stderrTail, fmt.Errorf("ddrescue rescued ratio %.4f is below minimum %.2f threshold", rescuedRatio, MinRescuedRatio)
	}

	return rescuedRatio, stderrTail, nil
}

func (w *Worker) finalizeAndEject(ctx context.Context, devicePath string, job *Job) (*Job, error) {
	job.State = StateISOFinalized
	if err := WriteJobFile(job.JobDir, job); err != nil {
		return nil, err
	}

	// Immediately eject physical DVD before starting asynchronous TMDB + NFS transfer
	w.ejectBestEffort(ctx, devicePath)

	// Two-point probe timing (METADATA.md Section 1.1):
	// Re-probe the finalized local <job_dir>/disc.iso after ejecting if the initial /dev/sr0
	// probe failed or returned zero valid tracks.
	if metadata.NeedsTrackReprobe(job.TracksSeconds) || metadata.NeedsLabelReprobe(job.RawLabel) {
		if reprobe, _ := metadata.ProbeTarget(ctx, w.runCmd, w.dvdcssEnv(), job.ISOPath); reprobe != nil {
			updated := false
			if metadata.NeedsLabelReprobe(job.RawLabel) && reprobe.RawLabel != "" {
				job.RawLabel = reprobe.RawLabel
				updated = true
			}
			if metadata.NeedsTrackReprobe(job.TracksSeconds) && len(reprobe.TracksSeconds) > 0 {
				job.TracksSeconds = reprobe.TracksSeconds
				updated = true
			}
			if updated {
				_ = WriteJobFile(job.JobDir, job)
			}
		}
		// If a delayed host udev event closed the tray while the local ISO re-probe was running,
		// re-run eject so the tray remains open when the rip completes.
		if status, _, statusErr := w.queryDriveStatusFn()(devicePath); statusErr == nil && status == drive.CDS_DISC_OK {
			w.logf("[%s] Drive %s still reports %s after local ISO re-probe; re-ejecting",
				job.JobID, devicePath, drive.DriveStatusString(status))
			w.ejectBestEffort(ctx, devicePath)
		}
	}

	if w.OnJobFinalized != nil {
		w.OnJobFinalized(job)
	}
	return job, nil
}

func (w *Worker) validateISOSize(isoPath string) bool {
	info, err := os.Stat(isoPath)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Size() >= w.minISOBytes()
}

func (w *Worker) minISOBytes() int64 {
	if w.MinISOSize > 0 {
		return w.MinISOSize
	}
	return MinISOSizeBytes
}

func (w *Worker) dvdcssEnv() []string {
	cacheDir := filepath.Join(w.ScratchDir, ".dvdcss")
	return []string{
		"DVDCSS_METHOD=key",
		"DVDCSS_CACHE=" + cacheDir,
		"DVDCSS_VERBOSE=1",
	}
}

func (w *Worker) runCmd(ctx context.Context, env []string, name string, args ...string) (string, error) {
	runner := w.RunCommand
	if runner == nil {
		runner = DefaultCommandRunner
	}
	return runner(ctx, env, name, args...)
}

func (w *Worker) ejectBackoffs() []time.Duration {
	if w.EjectRetryBackoffs != nil {
		return w.EjectRetryBackoffs
	}
	return DefaultEjectRetryBackoffs
}

func (w *Worker) ejectBestEffort(ctx context.Context, devicePath string) {
	backoffs := w.ejectBackoffs()
	totalAttempts := len(backoffs) + 1
	queryStatus := w.queryDriveStatusFn()

	for attempt := 1; attempt <= totalAttempts; attempt++ {
		if attempt > 1 {
			backoff := backoffs[attempt-2]
			if err := w.sleep(ctx, backoff); err != nil {
				return
			}
		}

		drive.PrepareForEject(devicePath)
		out, err := w.runCmd(ctx, nil, "eject", devicePath)
		if err != nil {
			w.logf("Warning: failed to eject %s (attempt %d/%d): %v (output: %s)",
				devicePath, attempt, totalAttempts, err, tailString(out, 256))
			continue
		}

		status, _, statusErr := queryStatus(devicePath)
		if statusErr != nil || status == drive.CDS_NO_INFO {
			return
		}
		if status == drive.CDS_TRAY_OPEN || status == drive.CDS_NO_DISC {
			w.logf("Ejected %s (status=%s)", devicePath, drive.DriveStatusString(status))
			return
		}

		w.logf("Warning: eject %s attempt %d/%d exited 0 but drive status is still %s (%d)",
			devicePath, attempt, totalAttempts, drive.DriveStatusString(status), status)
	}
}

func (w *Worker) notifyFailure(ev FailureEvent) {
	if w.OnRipFailed != nil {
		w.OnRipFailed(ev)
	}
}

func (w *Worker) nowUTC() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *Worker) logf(format string, args ...any) {
	if w.Logger != nil {
		w.Logger.Printf(format, args...)
	}
}

func tailString(s string, maxBytes int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxBytes {
		return s
	}
	return s[len(s)-maxBytes:]
}
