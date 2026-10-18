package drive

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"
)

func TestPollerInitialStartupDiscPresent(t *testing.T) {
	ctx := context.Background()
	triggered := 0

	p := NewPoller("/dev/sr0", func(ctx context.Context, dev string) error {
		triggered++
		return nil
	})
	p.Logger = log.New(io.Discard, "", 0)
	p.QueryStatus = func(dev string) (int, int, error) {
		return CDS_DISC_OK, CDS_DATA_1, nil
	}
	p.ProbeFS = func(ctx context.Context, dev string) (string, error) {
		return "udf", nil
	}

	// Initial state must be CDS_UNKNOWN (-1)
	status, latched, _ := p.Status()
	if status != CDS_UNKNOWN || latched {
		t.Fatalf("expected initial status CDS_UNKNOWN (-1) and unlatched, got status=%d latched=%v", status, latched)
	}

	// First poll observes UNKNOWN -> CDS_DISC_OK and immediately triggers
	if err := p.Step(ctx); err != nil {
		t.Fatalf("Step failed: %v", err)
	}
	if triggered != 1 {
		t.Fatalf("expected 1 trigger on startup with disc present, got %d", triggered)
	}

	// Subsequent polls while disc remains CDS_DISC_OK must NOT re-trigger (single-trigger latch)
	for i := 0; i < 5; i++ {
		if err := p.Step(ctx); err != nil {
			t.Fatalf("Step %d failed: %v", i, err)
		}
	}
	if triggered != 1 {
		t.Fatalf("expected latch to prevent re-triggering on same disc, got %d triggers", triggered)
	}
}

func TestPollerSingleTriggerLatchAndResetOnNonReady(t *testing.T) {
	ctx := context.Background()
	triggered := 0
	currentDriveStatus := CDS_NO_DISC

	p := NewPoller("/dev/sr0", func(ctx context.Context, dev string) error {
		triggered++
		return nil
	})
	p.Logger = log.New(io.Discard, "", 0)
	p.QueryStatus = func(dev string) (int, int, error) {
		return currentDriveStatus, CDS_DATA_1, nil
	}
	p.ProbeFS = func(ctx context.Context, dev string) (string, error) {
		return "iso9660", nil
	}

	// 1. Empty drive
	_ = p.Step(ctx)
	if triggered != 0 {
		t.Fatalf("expected 0 triggers on empty drive, got %d", triggered)
	}

	// 2. Tray opens and disc spins up
	currentDriveStatus = CDS_TRAY_OPEN
	_ = p.Step(ctx)
	currentDriveStatus = CDS_DRIVE_NOT_READY
	_ = p.Step(ctx)
	if triggered != 0 {
		t.Fatalf("expected 0 triggers before CDS_DISC_OK, got %d", triggered)
	}

	// 3. Disc becomes ready -> triggers first rip
	currentDriveStatus = CDS_DISC_OK
	_ = p.Step(ctx)
	if triggered != 1 {
		t.Fatalf("expected 1 trigger on first disc ready, got %d", triggered)
	}

	// 4. Transient CDS_NO_INFO or ioctl error while disc is still in drive must NOT reset latch
	currentDriveStatus = CDS_NO_INFO
	_ = p.Step(ctx)
	currentDriveStatus = CDS_DISC_OK
	_ = p.Step(ctx)
	if triggered != 1 {
		t.Fatalf("expected CDS_NO_INFO not to reset latch, got %d triggers", triggered)
	}

	// 5. Tray opens (user swaps disc) -> resets latch
	currentDriveStatus = CDS_TRAY_OPEN
	_ = p.Step(ctx)
	_, latched, label := p.Status()
	if latched || label != "Tray Open" {
		t.Fatalf("expected unlatched and 'Tray Open', got latched=%v label=%q", latched, label)
	}

	// 6. Second disc ready -> triggers second rip
	currentDriveStatus = CDS_DISC_OK
	_ = p.Step(ctx)
	if triggered != 2 {
		t.Fatalf("expected 2 triggers after second disc insertion, got %d", triggered)
	}
}

func TestPollerMediaValidationRejectsAudioAndBlankDiscs(t *testing.T) {
	ctx := context.Background()
	triggered := 0
	ejected := 0
	currentDriveStatus := CDS_DISC_OK
	currentDiscStatus := CDS_AUDIO
	currentFS := ""
	currentFSErr := error(nil)

	p := NewPoller("/dev/sr0", func(ctx context.Context, dev string) error {
		triggered++
		return nil
	})
	p.Logger = log.New(io.Discard, "", 0)
	p.QueryStatus = func(dev string) (int, int, error) {
		return currentDriveStatus, currentDiscStatus, nil
	}
	p.ProbeFS = func(ctx context.Context, dev string) (string, error) {
		return currentFS, currentFSErr
	}
	p.EjectDisc = func(ctx context.Context, dev string) error {
		ejected++
		return nil
	}

	// Case 1: Audio CD -> rejected and ejected cleanly, rip not triggered
	if err := p.Step(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if triggered != 0 || ejected != 1 {
		t.Fatalf("expected audio CD to be ejected without ripping, got triggered=%d ejected=%d", triggered, ejected)
	}

	// Reset via tray open
	currentDriveStatus = CDS_TRAY_OPEN
	_ = p.Step(ctx)

	// Case 2: Blank disc (blkid returns error / empty FS) -> rejected and ejected cleanly
	currentDriveStatus = CDS_DISC_OK
	currentDiscStatus = CDS_DATA_1
	currentFS = ""
	currentFSErr = errors.New("exit status 2")
	if err := p.Step(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if triggered != 0 || ejected != 2 {
		t.Fatalf("expected blank disc to be ejected without ripping, got triggered=%d ejected=%d", triggered, ejected)
	}

	// Reset via no disc
	currentDriveStatus = CDS_NO_DISC
	_ = p.Step(ctx)

	// Case 3: Valid UDF DVD -> accepted and rip triggered
	currentDriveStatus = CDS_DISC_OK
	currentDiscStatus = CDS_DATA_1
	currentFS = "udf"
	currentFSErr = nil
	if err := p.Step(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if triggered != 1 || ejected != 2 {
		t.Fatalf("expected valid UDF disc to trigger rip without ejecting early, got triggered=%d ejected=%d", triggered, ejected)
	}
}

func TestDriveStatusStringMapping(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{CDS_NO_DISC, "Empty"},
		{CDS_TRAY_OPEN, "Tray Open"},
		{CDS_DRIVE_NOT_READY, "Spinning Up / Reading TOC"},
		{CDS_DISC_OK, "Disc Ready"},
		{CDS_NO_INFO, "Unknown"},
		{CDS_UNKNOWN, "Unknown"},
	}
	for _, tc := range cases {
		if got := DriveStatusString(tc.status); got != tc.want {
			t.Errorf("DriveStatusString(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestPollerRunContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()

	polls := 0
	p := NewPoller("/dev/sr0", nil)
	p.Logger = log.New(io.Discard, "", 0)
	p.PollInterval = 10 * time.Millisecond
	p.QueryStatus = func(dev string) (int, int, error) {
		polls++
		return CDS_NO_DISC, CDS_NO_INFO, nil
	}

	err := p.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if polls < 2 {
		t.Fatalf("expected at least 2 polls during Run, got %d", polls)
	}
}
