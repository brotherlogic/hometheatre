package history

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryStorePersistenceAndCap(t *testing.T) {
	scratchDir := t.TempDir()
	fixedTime := time.Date(2026, 10, 18, 15, 30, 0, 0, time.UTC)

	store, err := NewStore(scratchDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	store.Now = func() time.Time { return fixedTime }

	// Record 105 entries to verify the 100-entry cap
	for i := 1; i <= 105; i++ {
		err := store.Record(Entry{
			JobID:          fmt.Sprintf("job_%03d", i),
			RawLabel:       fmt.Sprintf("LABEL_%03d", i),
			Classification: "MOVIE",
			Status:         StatusCompleted,
			Stage:          "stage1_dvdbackup",
			ISOPath:        fmt.Sprintf("/mnt/nfs/movies/Movie %d/Movie %d.iso", i, i),
		})
		if err != nil {
			t.Fatalf("Record %d failed: %v", i, err)
		}
	}

	entries := store.Entries()
	if len(entries) != MaxEntries {
		t.Fatalf("expected %d entries, got %d", MaxEntries, len(entries))
	}
	// Newest entry (105) should be first, and entry 6 should be the oldest kept (100th)
	if entries[0].JobID != "job_105" {
		t.Fatalf("expected newest entry job_105 first, got %s", entries[0].JobID)
	}
	if entries[MaxEntries-1].JobID != "job_006" {
		t.Fatalf("expected oldest kept entry job_006 last, got %s", entries[MaxEntries-1].JobID)
	}

	// Verify temporary file was renamed away
	if _, err := os.Stat(filepath.Join(scratchDir, "history.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("expected history.json.tmp to not exist after atomic rename")
	}

	// Reload from disk to simulate container restart
	reloaded, err := NewStore(scratchDir)
	if err != nil {
		t.Fatalf("NewStore reload failed: %v", err)
	}
	reloadedEntries := reloaded.Entries()
	if len(reloadedEntries) != MaxEntries || reloadedEntries[0].JobID != "job_105" {
		t.Fatalf("unexpected reloaded entries: len=%d first=%+v", len(reloadedEntries), reloadedEntries[0])
	}
}

func TestHistoryStoreUpdateByISOPath(t *testing.T) {
	scratchDir := t.TempDir()
	store, err := NewStore(scratchDir)
	if err != nil {
		t.Fatal(err)
	}

	unidISO := "/mnt/nfs/unidentified/OFFICE_S2_D1_20261018T153000Z.iso"
	if err := store.Record(Entry{
		JobID:          "job_20261018T153000Z",
		RawLabel:       "OFFICE_S2_D1",
		Classification: "TV_SERIES",
		Status:         StatusUnidentified,
		Stage:          "stage1_dvdbackup",
		ISOPath:        unidISO,
		Reason:         "tv_series_requires_episode_confirmation",
	}); err != nil {
		t.Fatal(err)
	}

	finalTVISO := "/mnt/nfs/tv/The Office (2005)/Season 02/The Office - S02E01E02.iso"
	err = store.UpdateByISOPath("OFFICE_S2_D1_20261018T153000Z.iso", func(e *Entry) {
		e.Status = StatusCompleted
		e.Title = "The Office - S02E01E02"
		e.ISOPath = finalTVISO
		e.Reason = ""
	})
	if err != nil {
		t.Fatalf("UpdateByISOPath failed: %v", err)
	}

	entries := store.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Status != StatusCompleted || entries[0].ISOPath != finalTVISO || entries[0].Title != "The Office - S02E01E02" {
		t.Fatalf("unexpected updated entry: %+v", entries[0])
	}
}

func TestLogBuffer(t *testing.T) {
	buf := NewLogBuffer(3)
	_, _ = buf.Write([]byte("line 1\nline 2\n"))
	_, _ = buf.Write([]byte("line 3\nline 4\n"))

	lines := buf.Lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(lines), lines)
	}
	if lines[0] != "line 2" || lines[1] != "line 3" || lines[2] != "line 4" {
		t.Fatalf("unexpected lines: %v", lines)
	}
}
