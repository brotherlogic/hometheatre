package ripper

import (
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
			// Simulate lsdvd -x failing so fallback to dvdbackup -I is also exercised
			return "", errors.New("lsdvd failed")
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
	wantCommands := [][]string{
		{"blkid", "-o", "value", "-s", "LABEL", "/dev/sr0"},
		{"dvdbackup", "-M", "-r", "a", "-n", "dvd", "-i", "/dev/sr0", "-o", expectedJobDir},
		{"genisoimage", "-quiet", "-dvd-video", "-V", "SONY_ARCCOS_MOVIE", "-o", filepath.Join(expectedJobDir, "disc.iso"), filepath.Join(expectedJobDir, "dvd")},
		{"lsdvd", "-x", "/dev/sr0"},
		{"dvdbackup", "-I", "-i", "/dev/sr0"},
		{"ddrescue", "-b", "2048", "-n", "-N", "-K", "64Ki,1Mi", "-O", "-T", "2m", "/dev/sr0", filepath.Join(expectedJobDir, "disc.iso"), filepath.Join(expectedJobDir, "ddrescue.map")},
		{"eject", "/dev/sr0"},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("commands mismatch:\ngot:  %v\nwant: %v", commands, wantCommands)
	}
	if finalizedJob == nil || job.Stage != Stage2DDRescue || job.State != StateISOFinalized || job.RescuedRatio < 0.95 {
		t.Fatalf("unexpected finalized job after Stage 2: %+v", job)
	}
}

func TestWorkerStage2LowRescuedRatioCleansUpAndEjects(t *testing.T) {
	ctx := context.Background()
	scratchDir := t.TempDir()

	ejected := 0
	enqueued := 0
	var failedEvent *FailureEvent

	w := NewWorker(scratchDir, "/dev/sr0")
	w.Logger = log.New(io.Discard, "", 0)
	w.Now = func() time.Time { return time.Date(2026, 10, 18, 16, 15, 0, 0, time.UTC) }
	w.CheckSpace = func(dir string) (uint64, error) { return 30 * 1024 * 1024 * 1024, nil }
	w.RunCommand = func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		switch name {
		case "blkid":
			return "SCRATCHED_DISC\n", nil
		case "dvdbackup":
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
