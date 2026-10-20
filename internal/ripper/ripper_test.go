package ripper

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brotherlogic/hometheatre/internal/drive"
)

func TestSanitizeVolumeID(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"THE_MATRIX_16X9", "THE_MATRIX_16X9"},
		{"  The Matrix (1999) - Disc 1  ", "THE_MATRIX_1999_DISC_1"},
		{"", "DVD_VIDEO"},
		{"   ___---...   ", "DVD_VIDEO"},
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_EXTRA", "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"},
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZ01234_EXTRA", "ABCDEFGHIJKLMNOPQRSTUVWXYZ01234"},
	}

	for _, tc := range cases {
		got := SanitizeVolumeID(tc.raw)
		if got != tc.want {
			t.Errorf("SanitizeVolumeID(%q) = %q, want %q", tc.raw, got, tc.want)
		}
		if len(got) > MaxVolumeLabelLen {
			t.Errorf("SanitizeVolumeID(%q) length %d exceeds MaxVolumeLabelLen (%d)", tc.raw, len(got), MaxVolumeLabelLen)
		}
	}
}

func TestParseDDRescueMap(t *testing.T) {
	// 1. ARccOS disc with 98% rescued bytes (passes >= 95%)
	mapContent := `# Mapfile. Created by GNU ddrescue version 1.27
# Command line: ddrescue -b 2048 -n -N -K 64Ki,1Mi -O -T 2m /dev/sr0 disc.iso ddrescue.map
# current_pos   current_status  current_pass
0x1F400000     +               1
#      pos        size  status
0x00000000  0x10000000  +
0x10000000  0x00800000  -
0x10800000  0x17800000  +
`
	rescued, total, ratio, err := ParseDDRescueMap(strings.NewReader(mapContent))
	if err != nil {
		t.Fatalf("ParseDDRescueMap unexpected error: %v", err)
	}
	wantRescued := uint64(0x10000000 + 0x17800000)
	wantTotal := uint64(0x10000000 + 0x00800000 + 0x17800000)
	if rescued != wantRescued || total != wantTotal {
		t.Fatalf("got rescued=%#x total=%#x, want rescued=%#x total=%#x", rescued, total, wantRescued, wantTotal)
	}
	if ratio < 0.95 {
		t.Fatalf("expected ratio >= 0.95, got %.4f", ratio)
	}

	// 2. Heavily damaged disc with < 95% rescued bytes
	badMap := `# Mapfile
0x00000000  +  1
0x00000000  0x09000000  +
0x09000000  0x01000000  -
`
	_, _, badRatio, err := ParseDDRescueMap(strings.NewReader(badMap))
	if err != nil {
		t.Fatalf("unexpected error on badMap: %v", err)
	}
	if badRatio >= 0.95 {
		t.Fatalf("expected ratio < 0.95, got %.4f", badRatio)
	}

	// 3. Empty mapfile should return error
	if _, _, _, err := ParseDDRescueMap(strings.NewReader("# only comments\n")); err == nil {
		t.Fatalf("expected error on empty mapfile, got nil")
	}
}

func TestReconcileScratch(t *testing.T) {
	scratchDir := t.TempDir()

	// Job 1: Interrupted rip (state == "ripping") -> should be deleted
	job1Dir := filepath.Join(scratchDir, "job_20261018T140000Z")
	if err := os.MkdirAll(filepath.Join(job1Dir, "dvd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteJobFile(job1Dir, &Job{
		JobID:     "job_20261018T140000Z",
		StartedAt: "2026-10-18T14:00:00Z",
		State:     StateRipping,
		Stage:     Stage1DVDBackup,
		RawLabel:  "PARTIAL_RIP",
	}); err != nil {
		t.Fatal(err)
	}

	// Job 2: Finalized state in job.json, but disc.iso is too small (< 500 MiB) -> should be deleted
	job2Dir := filepath.Join(scratchDir, "job_20261018T150000Z")
	if err := os.MkdirAll(job2Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteJobFile(job2Dir, &Job{
		JobID:     "job_20261018T150000Z",
		StartedAt: "2026-10-18T15:00:00Z",
		State:     StateISOFinalized,
		Stage:     Stage1GenISOImage,
		RawLabel:  "TRUNCATED_ISO",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job2Dir, "disc.iso"), []byte("too small"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Job 3: Valid finalized ISO (>= 500 MiB sparse file) -> should be preserved and re-enqueued
	job3Dir := filepath.Join(scratchDir, "job_20261018T160000Z")
	if err := os.MkdirAll(job3Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteJobFile(job3Dir, &Job{
		JobID:         "job_20261018T160000Z",
		StartedAt:     "2026-10-18T16:00:00Z",
		State:         StateISOFinalized,
		Stage:         Stage1GenISOImage,
		RawLabel:      "THE_MATRIX_16X9",
		TracksSeconds: []float64{8160.2},
	}); err != nil {
		t.Fatal(err)
	}
	createSparseISO(t, filepath.Join(job3Dir, "disc.iso"), MinISOSizeBytes+1024)

	var enqueued []*Job
	recovered, err := ReconcileScratch(scratchDir, func(j *Job) {
		enqueued = append(enqueued, j)
	})
	if err != nil {
		t.Fatalf("ReconcileScratch failed: %v", err)
	}

	if _, err := os.Stat(job1Dir); !os.IsNotExist(err) {
		t.Errorf("expected interrupted job1Dir to be deleted, stat err=%v", err)
	}
	if _, err := os.Stat(job2Dir); !os.IsNotExist(err) {
		t.Errorf("expected truncated job2Dir to be deleted, stat err=%v", err)
	}
	if _, err := os.Stat(job3Dir); err != nil {
		t.Errorf("expected valid finalized job3Dir to be preserved, stat err=%v", err)
	}
	if len(recovered) != 1 || len(enqueued) != 1 || enqueued[0].JobID != "job_20261018T160000Z" {
		t.Fatalf("expected 1 recovered job (job_20261018T160000Z), got recovered=%d enqueued=%d", len(recovered), len(enqueued))
	}
}

func TestWorkerSpaceGateWaitsForActiveTransferAndAbortsWhenNone(t *testing.T) {
	ctx := context.Background()
	scratchDir := t.TempDir()

	// Case A: < 18 GiB free with NO pending transfer -> aborts with ErrInsufficientScratchSpace and ejects
	ejected := 0
	var failedEvent *FailureEvent
	w := NewWorker(scratchDir, "/dev/sr0")
	w.Logger = log.New(io.Discard, "", 0)
	w.CheckSpace = func(dir string) (uint64, error) {
		return 10 * 1024 * 1024 * 1024, nil // 10 GiB (< 18 GiB)
	}
	w.HasPendingTransfers = func() bool { return false }
	w.RunCommand = func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		if name == "eject" {
			ejected++
		}
		return "", nil
	}
	w.OnRipFailed = func(ev FailureEvent) {
		failedEvent = &ev
	}

	_, err := w.RipDisc(ctx, "/dev/sr0")
	if !errors.Is(err, ErrInsufficientScratchSpace) {
		t.Fatalf("expected ErrInsufficientScratchSpace, got %v", err)
	}
	if ejected != 1 {
		t.Fatalf("expected disc to be ejected on insufficient scratch space, got ejected=%d", ejected)
	}
	if failedEvent == nil || failedEvent.Reason != "insufficient_scratch_space" {
		t.Fatalf("expected OnRipFailed with insufficient_scratch_space, got %+v", failedEvent)
	}

	// Case B: < 18 GiB free while an NFS transfer is active -> waits in "Waiting for Scratch Space" until space frees
	var spaceChecks int32
	observedWaitStatus := false
	w2 := NewWorker(scratchDir, "/dev/sr0")
	w2.Logger = log.New(io.Discard, "", 0)
	w2.SpaceWaitInterval = 5 * time.Millisecond
	w2.MinISOSize = 1024
	w2.CheckSpace = func(dir string) (uint64, error) {
		n := atomic.AddInt32(&spaceChecks, 1)
		if n < 3 {
			return 12 * 1024 * 1024 * 1024, nil // 12 GiB while transfer in flight
		}
		return 25 * 1024 * 1024 * 1024, nil // 25 GiB once transfer finishes
	}
	w2.HasPendingTransfers = func() bool {
		if w2.Status() == StatusWaitingForScratch {
			observedWaitStatus = true
		}
		return atomic.LoadInt32(&spaceChecks) < 3
	}
	w2.RunCommand = func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		switch name {
		case "blkid":
			return "THE_MATRIX_16X9\n", nil
		case "dvdbackup":
			dvdDir := filepath.Join(args[len(args)-1], "dvd", "VIDEO_TS")
			_ = os.MkdirAll(dvdDir, 0o755)
			return "", nil
		case "genisoimage":
			outISO := args[5]
			createSparseISO(t, outISO, 2048)
			return "", nil
		case "eject":
			return "", nil
		}
		return "", nil
	}

	job, err := w2.RipDisc(ctx, "/dev/sr0")
	if err != nil {
		t.Fatalf("expected RipDisc to succeed after space freed, got %v", err)
	}
	if !observedWaitStatus {
		t.Fatalf("expected worker to enter %q while waiting for transfer", StatusWaitingForScratch)
	}
	if job.State != StateISOFinalized {
		t.Fatalf("expected job state %q, got %q", StateISOFinalized, job.State)
	}
}

func TestWorkerStage1SuccessFlow(t *testing.T) {
	ctx := context.Background()
	scratchDir := t.TempDir()

	var commands [][]string
	var dvdcssEnvSeen []string
	var finalizedJob *Job
	ejectedBeforeFinalizeCallback := false
	ejected := false

	w := NewWorker(scratchDir, "/dev/sr0")
	w.Logger = log.New(io.Discard, "", 0)
	w.Now = func() time.Time { return time.Date(2026, 10, 18, 15, 30, 0, 0, time.UTC) }
	w.CheckSpace = func(dir string) (uint64, error) { return 30 * 1024 * 1024 * 1024, nil }
	w.RunCommand = func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		commands = append(commands, append([]string{name}, args...))
		switch name {
		case "blkid":
			return "THE_MATRIX_16X9\n", nil
		case "lsdvd":
			return `<?xml version="1.0" encoding="UTF-8"?><lsdvd><title>THE_MATRIX_16X9</title><track><ix>1</ix><length>8160.200</length></track></lsdvd>`, nil
		case "dvdbackup":
			dvdcssEnvSeen = env
			jobDir := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(jobDir, "dvd", "VIDEO_TS"), 0o755); err != nil {
				return "", err
			}
			return "", nil
		case "genisoimage":
			outISO := args[5]
			createSparseISO(t, outISO, MinISOSizeBytes+4096)
			return "", nil
		case "eject":
			ejected = true
			return "", nil
		default:
			return "", nil
		}
	}
	w.OnJobFinalized = func(j *Job) {
		ejectedBeforeFinalizeCallback = ejected
		finalizedJob = j
	}

	job, err := w.RipDisc(ctx, "/dev/sr0")
	if err != nil {
		t.Fatalf("RipDisc Stage 1 failed: %v", err)
	}

	expectedJobDir := filepath.Join(scratchDir, "job_20261018T153000Z")
	wantCommands := [][]string{
		{"blkid", "-o", "value", "-s", "LABEL", "/dev/sr0"},
		{"lsdvd", "-Ox", "/dev/sr0"},
		{"dvdbackup", "-M", "-r", "a", "-n", "dvd", "-i", "/dev/sr0", "-o", expectedJobDir},
		{"genisoimage", "-quiet", "-dvd-video", "-V", "THE_MATRIX_16X9", "-o", filepath.Join(expectedJobDir, "disc.iso"), filepath.Join(expectedJobDir, "dvd")},
		{"eject", "/dev/sr0"},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("commands mismatch:\ngot:  %v\nwant: %v", commands, wantCommands)
	}

	// Verify libdvdcss2 environment variables were passed
	wantEnv := []string{
		"DVDCSS_METHOD=key",
		"DVDCSS_CACHE=" + filepath.Join(scratchDir, ".dvdcss"),
		"DVDCSS_VERBOSE=1",
	}
	if !reflect.DeepEqual(dvdcssEnvSeen, wantEnv) {
		t.Fatalf("dvdcss env mismatch: got %v, want %v", dvdcssEnvSeen, wantEnv)
	}

	// Verify <job_dir>/dvd was deleted after genisoimage
	if _, err := os.Stat(filepath.Join(expectedJobDir, "dvd")); !os.IsNotExist(err) {
		t.Fatalf("expected <job_dir>/dvd to be deleted after genisoimage, stat err=%v", err)
	}

	// Verify job.json on disk is finalized and eject ran before enqueueing
	persisted, err := ReadJobFile(expectedJobDir)
	if err != nil {
		t.Fatalf("ReadJobFile failed: %v", err)
	}
	if persisted.State != StateISOFinalized || persisted.Stage != Stage1GenISOImage {
		t.Fatalf("unexpected persisted job state/stage: %+v", persisted)
	}
	if len(persisted.TracksSeconds) != 1 || persisted.TracksSeconds[0] != 8160.2 {
		t.Fatalf("expected persisted TracksSeconds=[8160.2], got %v", persisted.TracksSeconds)
	}
	if !ejectedBeforeFinalizeCallback || finalizedJob == nil || finalizedJob.JobID != job.JobID {
		t.Fatalf("expected physical disc to be ejected immediately and job enqueued")
	}
}

func TestWorkerStage1GenISOFailureFallsBackToStage2DDRescue(t *testing.T) {
	ctx := context.Background()
	scratchDir := t.TempDir()

	var commands [][]string
	var finalizedJob *Job

	w := NewWorker(scratchDir, "/dev/sr0")
	w.Logger = log.New(io.Discard, "", 0)
	w.Now = func() time.Time { return time.Date(2026, 10, 18, 16, 0, 0, 0, time.UTC) }
	w.CheckSpace = func(dir string) (uint64, error) { return 30 * 1024 * 1024 * 1024, nil }
	w.RunCommand = func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		commands = append(commands, append([]string{name}, args...))
		switch name {
		case "blkid":
			return "SONY_ARCCOS_MOVIE\n", nil
		case "dvdbackup":
			if len(args) > 0 && args[0] == "-M" {
				jobDir := args[len(args)-1]
				_ = os.MkdirAll(filepath.Join(jobDir, "dvd", "VIDEO_TS"), 0o755)
				return "", nil
			}
			// Stage 2A fallback: dvdbackup -I -i /dev/sr0
			return "", nil
		case "genisoimage":
			// Simulate genisoimage failing due to overlapping ARccOS IFO pointers
			return "genisoimage: Inode number overflow / malformed IFO table", errors.New("exit status 255")
		case "lsdvd":
			// Simulate lsdvd failing on physical /dev/sr0 (exercising both dvdbackup -I CSS fallback
			// and automatic post-eject re-probe of the finalized local disc.iso)
			if len(args) >= 2 && strings.HasSuffix(args[len(args)-1], "disc.iso") {
				return `<?xml version="1.0" encoding="UTF-8"?><lsdvd><title>SONY_ARCCOS_MOVIE</title><track><ix>1</ix><length>7320.500</length></track></lsdvd>`, nil
			}
			return "", errors.New("lsdvd failed on physical drive")
		case "ddrescue":
			isoPath := args[len(args)-2]
			mapPath := args[len(args)-1]
			createSparseISO(t, isoPath, MinISOSizeBytes+1024)
			mapData := `# Mapfile
0x10000000 + 1
0x00000000  0x1F000000  +
0x1F000000  0x00400000  -
`
			if err := os.WriteFile(mapPath, []byte(mapData), 0o644); err != nil {
				return "", err
			}
			return "", nil
		case "eject":
			return "", nil
		default:
			return "", nil
		}
	}
	w.OnJobFinalized = func(j *Job) {
		finalizedJob = j
	}

	job, err := w.RipDisc(ctx, "/dev/sr0")
	if err != nil {
		t.Fatalf("expected Stage 2 fallback to succeed, got %v", err)
	}

	expectedJobDir := filepath.Join(scratchDir, "job_20261018T160000Z")
	expectedISOPath := filepath.Join(expectedJobDir, "disc.iso")
	wantCommands := [][]string{
		{"blkid", "-o", "value", "-s", "LABEL", "/dev/sr0"},
		{"lsdvd", "-Ox", "/dev/sr0"},
		{"dvdbackup", "-M", "-r", "a", "-n", "dvd", "-i", "/dev/sr0", "-o", expectedJobDir},
		{"genisoimage", "-quiet", "-dvd-video", "-V", "SONY_ARCCOS_MOVIE", "-o", expectedISOPath, filepath.Join(expectedJobDir, "dvd")},
		{"lsdvd", "-x", "/dev/sr0"},
		{"dvdbackup", "-I", "-i", "/dev/sr0"},
		{"ddrescue", "-b", "2048", "-n", "-N", "-K", "64Ki,1Mi", "-O", "-T", "2m", "/dev/sr0", expectedISOPath, filepath.Join(expectedJobDir, "ddrescue.map")},
		{"eject", "/dev/sr0"},
		{"blkid", "-o", "value", "-s", "LABEL", expectedISOPath},
		{"lsdvd", "-Ox", expectedISOPath},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("commands mismatch:\ngot:  %v\nwant: %v", commands, wantCommands)
	}
	if finalizedJob == nil || job.Stage != Stage2DDRescue || job.State != StateISOFinalized || job.RescuedRatio < 0.95 {
		t.Fatalf("unexpected finalized job after Stage 2: %+v", job)
	}
	if len(job.TracksSeconds) != 1 || job.TracksSeconds[0] != 7320.5 {
		t.Fatalf("expected re-probed TracksSeconds=[7320.5], got %v", job.TracksSeconds)
	}
}

func TestWorkerStage2LowRescuedRatioCleansUpAndEjects(t *testing.T) {
	ctx := context.Background()
	scratchDir := t.TempDir()

	ejected := 0
	enqueued := 0
	dvdbackupAttempts := 0
	var sleepDurations []time.Duration
	var failedEvent *FailureEvent

	w := NewWorker(scratchDir, "/dev/sr0")
	w.Logger = log.New(io.Discard, "", 0)
	w.Now = func() time.Time { return time.Date(2026, 10, 18, 16, 15, 0, 0, time.UTC) }
	w.CheckSpace = func(dir string) (uint64, error) { return 30 * 1024 * 1024 * 1024, nil }
	w.QueryDriveStatus = func(dev string) (int, int, error) {
		if ejected > 0 {
			return drive.CDS_TRAY_OPEN, drive.CDS_NO_INFO, nil
		}
		return drive.CDS_DISC_OK, drive.CDS_DATA_1, nil
	}
	w.Sleep = func(ctx context.Context, d time.Duration) error {
		sleepDurations = append(sleepDurations, d)
		return nil
	}
	w.RunCommand = func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		switch name {
		case "blkid":
			return "SCRATCHED_DISC\n", nil
		case "dvdbackup":
			if len(args) > 0 && args[0] == "-M" {
				dvdbackupAttempts++
			}
			return "dvdbackup: read error", errors.New("exit status 1")
		case "lsdvd":
			return "", nil
		case "ddrescue":
			isoPath := args[len(args)-2]
			mapPath := args[len(args)-1]
			createSparseISO(t, isoPath, MinISOSizeBytes+1024)
			// Only 80% rescued (< 95% threshold)
			mapData := `# Mapfile
0x10000000 + 1
0x00000000  0x08000000  +
0x08000000  0x02000000  -
`
			_ = os.WriteFile(mapPath, []byte(mapData), 0o644)
			return "ddrescue: read errors encountered", nil
		case "eject":
			ejected++
			return "", nil
		default:
			return "", nil
		}
	}
	w.OnJobFinalized = func(j *Job) {
		enqueued++
	}
	w.OnRipFailed = func(ev FailureEvent) {
		failedEvent = &ev
	}

	_, err := w.RipDisc(ctx, "/dev/sr0")
	if err == nil {
		t.Fatalf("expected RipDisc to fail when rescuedRatio < 0.95")
	}
	if dvdbackupAttempts != 3 {
		t.Fatalf("expected 3 Stage 1 dvdbackup attempts before falling back to Stage 2, got %d", dvdbackupAttempts)
	}
	if !reflect.DeepEqual(sleepDurations, DefaultStage1RetryBackoffs) {
		t.Fatalf("expected Stage 1 retry backoffs %v, got %v", DefaultStage1RetryBackoffs, sleepDurations)
	}
	if enqueued != 0 {
		t.Fatalf("expected broken ISO not to be enqueued, got enqueued=%d", enqueued)
	}
	if ejected != 1 {
		t.Fatalf("expected disc to be ejected on Stage 2 failure, got ejected=%d", ejected)
	}
	expectedJobDir := filepath.Join(scratchDir, "job_20261018T161500Z")
	if _, statErr := os.Stat(expectedJobDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected failed jobDir %s to be deleted from scratch, statErr=%v", expectedJobDir, statErr)
	}
	if failedEvent == nil || failedEvent.Stage != Stage2DDRescue || failedEvent.RescuedRatio >= 0.95 {
		t.Fatalf("unexpected FailureEvent: %+v", failedEvent)
	}
}

func TestWorkerStage1RetriesDVDBackupAfterTransientDriveReset(t *testing.T) {
	ctx := context.Background()
	scratchDir := t.TempDir()

	var logBuf bytes.Buffer
	dvdbackupAttempts := 0
	statusChecks := 0
	ejected := 0
	ddrescueCalled := false

	w := NewWorker(scratchDir, "/dev/sr0")
	w.Logger = log.New(&logBuf, "", 0)
	w.Now = func() time.Time { return time.Date(2026, 10, 18, 22, 59, 26, 0, time.UTC) }
	w.CheckSpace = func(dir string) (uint64, error) { return 30 * 1024 * 1024 * 1024, nil }
	w.Stage1RetryBackoffs = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	w.QueryDriveStatus = func(dev string) (int, int, error) {
		if ejected > 0 {
			return drive.CDS_TRAY_OPEN, drive.CDS_NO_INFO, nil
		}
		statusChecks++
		if statusChecks == 1 {
			// Simulate drive still settling / spinning up on first readiness check
			return drive.CDS_DRIVE_NOT_READY, drive.CDS_NO_INFO, nil
		}
		return drive.CDS_DISC_OK, drive.CDS_DATA_1, nil
	}
	w.RunCommand = func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		switch name {
		case "blkid":
			return "THE_MATRIX_16X9\n", nil
		case "lsdvd":
			return "", errors.New("libdvdread: Can't open /dev/sr0 for reading")
		case "dvdbackup":
			dvdbackupAttempts++
			if dvdbackupAttempts == 1 {
				return "libdvdread: DVDOpenFileUDF:UDFFindFile /VIDEO_TS/VIDEO_TS.IFO failed\nCannot open Video Manager (VMG) info.\nMirror of DVD failed",
					errors.New("signal: segmentation fault (core dumped)")
			}
			jobDir := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(jobDir, "dvd", "VIDEO_TS"), 0o755); err != nil {
				return "", err
			}
			return "", nil
		case "genisoimage":
			outISO := args[5]
			createSparseISO(t, outISO, MinISOSizeBytes+4096)
			return "", nil
		case "ddrescue":
			ddrescueCalled = true
			return "", nil
		case "eject":
			ejected++
			return "", nil
		default:
			return "", nil
		}
	}

	job, err := w.RipDisc(ctx, "/dev/sr0")
	if err != nil {
		t.Fatalf("expected Stage 1 retry to succeed, got %v", err)
	}
	if dvdbackupAttempts != 2 {
		t.Fatalf("expected dvdbackup to succeed on attempt 2, got %d attempts", dvdbackupAttempts)
	}
	if statusChecks < 2 {
		t.Fatalf("expected WaitForDiscReady to poll until CDS_DISC_OK, got %d status checks", statusChecks)
	}
	if ddrescueCalled {
		t.Fatalf("expected Stage 2 ddrescue not to be called when Stage 1 retry succeeds")
	}
	if job.Stage != Stage1GenISOImage || job.State != StateISOFinalized {
		t.Fatalf("unexpected job state/stage: %+v", job)
	}

	logs := logBuf.String()
	for _, wantLog := range []string{
		`Initial probe on /dev/sr0: label="THE_MATRIX_16X9" (safe="THE_MATRIX_16X9"), tracks=0, err=libdvdread: Can't open /dev/sr0 for reading`,
		`Stage 1 dvdbackup attempt 1/3 failed`,
		`Drive /dev/sr0 ready (CDS_DISC_OK); retrying Stage 1 dvdbackup (attempt 2/3)`,
		`Ejected /dev/sr0 (status=Tray Open)`,
	} {
		if !strings.Contains(logs, wantLog) {
			t.Errorf("expected daemon logs to contain %q, got:\n%s", wantLog, logs)
		}
	}
}

func TestQueryDiscCapacityAndSysfsFallback(t *testing.T) {
	// 1. Verify sysfs /sys/class/block/sr0/size parsing (512-byte sectors)
	sysfsRoot := t.TempDir()
	sr0Dir := filepath.Join(sysfsRoot, "sr0")
	if err := os.MkdirAll(sr0Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 16,000,000 * 512 = 8,192,000,000 bytes (~7.63 GiB)
	if err := os.WriteFile(filepath.Join(sr0Dir, "size"), []byte("16000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gotSysfs, err := readSysfsBlockCapacity(sysfsRoot, "/dev/sr0")
	if err != nil {
		t.Fatalf("readSysfsBlockCapacity unexpected error: %v", err)
	}
	wantSysfs := int64(16000000) * 512
	if gotSysfs != wantSysfs {
		t.Fatalf("readSysfsBlockCapacity = %d, want %d", gotSysfs, wantSysfs)
	}

	// 2. Verify QueryDiscCapacity falls back to regular file size
	fakeDev := filepath.Join(t.TempDir(), "fake_disc.iso")
	createSparseISO(t, fakeDev, 4*1024*1024*1024)
	gotFile, err := QueryDiscCapacity(fakeDev)
	if err != nil {
		t.Fatalf("QueryDiscCapacity on regular file failed: %v", err)
	}
	if gotFile != 4*1024*1024*1024 {
		t.Fatalf("QueryDiscCapacity = %d, want %d", gotFile, int64(4*1024*1024*1024))
	}
}

func TestWorkerLiveProgressAcrossStages(t *testing.T) {
	ctx := context.Background()
	scratchDir := t.TempDir()

	const totalDiscBytes int64 = 4 * 1024 * 1024 * 1024 // 4 GiB
	const stage1VOBBytes int64 = 2 * 1024 * 1024 * 1024 // 2 GiB written in VIDEO_TS
	const stage1ISOBytes int64 = 1 * 1024 * 1024 * 1024 // 1 GiB written so far to disc.iso

	var stage1AProg, stage1BProg, stage2MapProg, stage2ISOFallbackProg Progress

	w := NewWorker(scratchDir, "/dev/sr0")
	w.Logger = log.New(io.Discard, "", 0)
	w.Now = func() time.Time { return time.Date(2026, 10, 18, 23, 50, 0, 0, time.UTC) }
	w.CheckSpace = func(dir string) (uint64, error) { return 30 * 1024 * 1024 * 1024, nil }
	w.QueryDiscSize = func(dev string) (int64, error) { return totalDiscBytes, nil }

	// Before ripping starts, Progress().Active must be false
	if p := w.Progress(); p.Active {
		t.Fatalf("expected Progress().Active == false when idle, got %+v", p)
	}

	w.RunCommand = func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		switch name {
		case "blkid":
			return "THE_MATRIX_16X9\n", nil
		case "lsdvd":
			return `<?xml version="1.0" encoding="UTF-8"?><lsdvd><title>THE_MATRIX_16X9</title><track><ix>1</ix><length>8160.200</length></track></lsdvd>`, nil
		case "dvdbackup":
			if len(args) > 0 && args[0] == "-M" {
				jobDir := args[len(args)-1]
				videoTS := filepath.Join(jobDir, "dvd", "VIDEO_TS")
				if err := os.MkdirAll(videoTS, 0o755); err != nil {
					return "", err
				}
				createSparseISO(t, filepath.Join(videoTS, "VTS_01_1.VOB"), stage1VOBBytes/2)
				createSparseISO(t, filepath.Join(videoTS, "VTS_01_2.VOB"), stage1VOBBytes/2)
				stage1AProg = w.Progress()
				return "", nil
			}
			return "", nil
		case "genisoimage":
			outISO := args[5]
			createSparseISO(t, outISO, stage1ISOBytes)
			stage1BProg = w.Progress()
			// Return error to exercise Stage 2 ddrescue progress tracking in the same run
			return "genisoimage: simulated failure to test Stage 2 progress", errors.New("exit status 1")
		case "ddrescue":
			isoPath := args[len(args)-2]
			mapPath := args[len(args)-1]
			// First check fallback when only disc.iso exists before ddrescue.map is flushed
			createSparseISO(t, isoPath, 3*1024*1024*1024)
			stage2ISOFallbackProg = w.Progress()

			// Now write ddrescue.map with 0xC0000000 (3 GiB) rescued out of 0x100000000 (4 GiB)
			mapData := `# Mapfile
0xC0000000 + 1
0x00000000  0xC0000000  +
0xC0000000  0x40000000  ?
`
			if err := os.WriteFile(mapPath, []byte(mapData), 0o644); err != nil {
				return "", err
			}
			stage2MapProg = w.Progress()

			// Finalize mapfile to >= 95% rescued so RipDisc succeeds
			finalMap := `# Mapfile
0x100000000 + 1
0x00000000  0xFC000000  +
0xFC000000  0x04000000  -
`
			_ = os.WriteFile(mapPath, []byte(finalMap), 0o644)
			return "", nil
		case "eject":
			return "", nil
		default:
			return "", nil
		}
	}

	if _, err := w.RipDisc(ctx, "/dev/sr0"); err != nil {
		t.Fatalf("RipDisc failed: %v", err)
	}

	// Verify Stage 1A (stage1_dvdbackup) progress: 2 GiB / 4 GiB = 50.0%
	if !stage1AProg.Active || stage1AProg.Stage != Stage1DVDBackup || stage1AProg.Label != "THE_MATRIX_16X9" {
		t.Fatalf("unexpected Stage 1A progress metadata: %+v", stage1AProg)
	}
	if stage1AProg.BytesDone != stage1VOBBytes || stage1AProg.BytesTotal != totalDiscBytes || stage1AProg.Percent != 50.0 {
		t.Fatalf("Stage 1A progress got %+v, want BytesDone=%d BytesTotal=%d Percent=50.0", stage1AProg, stage1VOBBytes, totalDiscBytes)
	}

	// Verify Stage 1B (stage1_genisoimage) progress: 1 GiB ISO / 2 GiB extracted <job_dir>/dvd = 50.0%
	if !stage1BProg.Active || stage1BProg.Stage != Stage1GenISOImage {
		t.Fatalf("unexpected Stage 1B progress metadata: %+v", stage1BProg)
	}
	if stage1BProg.BytesDone != stage1ISOBytes || stage1BProg.BytesTotal != stage1VOBBytes || stage1BProg.Percent != 50.0 {
		t.Fatalf("Stage 1B progress got %+v, want BytesDone=%d BytesTotal=%d Percent=50.0", stage1BProg, stage1ISOBytes, stage1VOBBytes)
	}

	// Verify Stage 2 (stage2_ddrescue) progress via disc.iso fallback and ddrescue.map: 3 GiB / 4 GiB = 75.0%
	wantStage2Done := int64(3 * 1024 * 1024 * 1024)
	if !stage2ISOFallbackProg.Active || stage2ISOFallbackProg.BytesDone != wantStage2Done || stage2ISOFallbackProg.Percent != 75.0 {
		t.Fatalf("Stage 2 ISO fallback progress got %+v, want BytesDone=%d Percent=75.0", stage2ISOFallbackProg, wantStage2Done)
	}
	if !stage2MapProg.Active || stage2MapProg.Stage != Stage2DDRescue || stage2MapProg.BytesDone != wantStage2Done || stage2MapProg.BytesTotal != totalDiscBytes || stage2MapProg.Percent != 75.0 {
		t.Fatalf("Stage 2 map progress got %+v, want BytesDone=%d BytesTotal=%d Percent=75.0", stage2MapProg, wantStage2Done, totalDiscBytes)
	}

	// After RipDisc returns, Progress().Active must reset to false
	if p := w.Progress(); p.Active {
		t.Fatalf("expected Progress().Active == false after RipDisc completes, got %+v", p)
	}
}

func TestWorkerEjectRetriesWhenTrayStaysClosed(t *testing.T) {
	ctx := context.Background()
	scratchDir := t.TempDir()

	var logBuf bytes.Buffer
	var sleepDurations []time.Duration
	ejectCalls := 0

	w := NewWorker(scratchDir, "/dev/sr0")
	w.Logger = log.New(&logBuf, "", 0)
	w.Now = func() time.Time { return time.Date(2026, 10, 19, 0, 18, 48, 0, time.UTC) }
	w.CheckSpace = func(dir string) (uint64, error) { return 30 * 1024 * 1024 * 1024, nil }
	w.Sleep = func(ctx context.Context, d time.Duration) error {
		sleepDurations = append(sleepDurations, d)
		return nil
	}
	// Simulate eject exiting 0 on attempt 1 while the tray stays closed (CDS_DISC_OK) due to
	// host udev/spin-down race, then opening (CDS_TRAY_OPEN) on attempt 2.
	w.QueryDriveStatus = func(dev string) (int, int, error) {
		if ejectCalls >= 2 {
			return drive.CDS_TRAY_OPEN, drive.CDS_NO_INFO, nil
		}
		return drive.CDS_DISC_OK, drive.CDS_DATA_1, nil
	}
	w.RunCommand = func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		switch name {
		case "blkid":
			return "MARATHON_MAN\n", nil
		case "lsdvd":
			return `<?xml version="1.0" encoding="UTF-8"?><lsdvd><title>MARATHON_MAN</title><track><ix>1</ix><length>7500.000</length></track></lsdvd>`, nil
		case "dvdbackup":
			jobDir := args[len(args)-1]
			_ = os.MkdirAll(filepath.Join(jobDir, "dvd", "VIDEO_TS"), 0o755)
			return "", nil
		case "genisoimage":
			outISO := args[5]
			createSparseISO(t, outISO, MinISOSizeBytes+4096)
			return "", nil
		case "eject":
			ejectCalls++
			return "", nil
		default:
			return "", nil
		}
	}

	if _, err := w.RipDisc(ctx, "/dev/sr0"); err != nil {
		t.Fatalf("RipDisc failed: %v", err)
	}
	if ejectCalls != 2 {
		t.Fatalf("expected 2 eject calls when first attempt leaves drive in CDS_DISC_OK, got %d", ejectCalls)
	}
	wantSleeps := []time.Duration{DefaultEjectRetryBackoffs[0]}
	if !reflect.DeepEqual(sleepDurations, wantSleeps) {
		t.Fatalf("expected eject retry backoff %v, got %v", wantSleeps, sleepDurations)
	}

	logs := logBuf.String()
	for _, wantLog := range []string{
		`Warning: eject /dev/sr0 attempt 1/3 exited 0 but drive status is still Disc Ready (4)`,
		`Ejected /dev/sr0 (status=Tray Open)`,
	} {
		if !strings.Contains(logs, wantLog) {
			t.Errorf("expected logs to contain %q, got:\n%s", wantLog, logs)
		}
	}
}

func createSparseISO(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open sparse ISO %s: %v", path, err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatalf("truncate sparse ISO %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close sparse ISO %s: %v", path, err)
	}
}
