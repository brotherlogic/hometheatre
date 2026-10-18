package nfs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brotherlogic/hometheatre/internal/metadata"
	"github.com/brotherlogic/hometheatre/internal/ripper"
)

func TestWithNFSUserAndAtomicMovieTransfer(t *testing.T) {
	ctx := context.Background()
	scratchDir := t.TempDir()
	nfsRoot := t.TempDir()

	srv := newMockTMDBServer(t)
	defer srv.Close()

	tmdb := metadata.NewTMDBClient("0123456789abcdef0123456789abcdef")
	tmdb.BaseURL = srv.URL + "/3"

	jobDir := filepath.Join(scratchDir, "job_20261018T153000Z")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	isoPath := filepath.Join(jobDir, "disc.iso")
	isoContent := []byte("simulated-decrypted-dvd-iso-payload")
	if err := os.WriteFile(isoPath, isoContent, 0o644); err != nil {
		t.Fatal(err)
	}
	job := &ripper.Job{
		JobID:         "job_20261018T153000Z",
		StartedAt:     "2026-10-18T15:30:00Z",
		State:         ripper.StateISOFinalized,
		Stage:         ripper.Stage1GenISOImage,
		RawLabel:      "THE_MATRIX_16X9",
		TracksSeconds: []float64{8160.0},
		JobDir:        jobDir,
		ISOPath:       isoPath,
	}
	if err := ripper.WriteJobFile(jobDir, job); err != nil {
		t.Fatal(err)
	}

	var nfsUserCalledWithPUID, nfsUserCalledWithPGID int
	var copyBufLen int
	var nfoExistedDuringISOCopy bool
	var partialExistedDuringISOCopy bool
	var finalISOExistedDuringCopy bool

	writer := NewWriter(nfsRoot, 1000, 1000, tmdb)
	writer.Logger = log.New(io.Discard, "", 0)
	writer.NFSUserRunner = func(puid, pgid int, fn func() error) error {
		nfsUserCalledWithPUID = puid
		nfsUserCalledWithPGID = pgid
		return WithNFSUser(puid, pgid, fn)
	}
	writer.CopyHook = func(dst io.Writer, src io.Reader, buf []byte) (int64, error) {
		copyBufLen = len(buf)
		movieDir := filepath.Join(nfsRoot, "movies", "The Matrix (1999)")
		if _, err := os.Stat(filepath.Join(movieDir, "The Matrix (1999).nfo")); err == nil {
			nfoExistedDuringISOCopy = true
		}
		if _, err := os.Stat(filepath.Join(movieDir, ".The Matrix (1999).iso.partial")); err == nil {
			partialExistedDuringISOCopy = true
		}
		if _, err := os.Stat(filepath.Join(movieDir, "The Matrix (1999).iso")); err == nil {
			finalISOExistedDuringCopy = true
		}
		return io.CopyBuffer(dst, src, buf)
	}

	outcome, err := writer.ProcessJobOnce(ctx, job)
	if err != nil {
		t.Fatalf("ProcessJobOnce failed: %v", err)
	}

	if nfsUserCalledWithPUID != 1000 || nfsUserCalledWithPGID != 1000 {
		t.Fatalf("expected WithNFSUser(1000, 1000), got (%d, %d)", nfsUserCalledWithPUID, nfsUserCalledWithPGID)
	}
	if copyBufLen != CopyBufferSize {
		t.Fatalf("expected 1 MiB (%d) CopyBuffer, got %d", CopyBufferSize, copyBufLen)
	}
	if !nfoExistedDuringISOCopy {
		t.Fatalf("expected .nfo file to be written and renamed BEFORE .iso copy")
	}
	if !partialExistedDuringISOCopy || finalISOExistedDuringCopy {
		t.Fatalf("expected .iso to be streamed to hidden .partial file before final rename")
	}
	if !outcome.AutoFiled {
		t.Fatalf("expected AutoFiled=true, got %+v", outcome)
	}

	// Verify final files on NFS and deletion of local scratch jobDir
	wantISO := filepath.Join(nfsRoot, "movies", "The Matrix (1999)", "The Matrix (1999).iso")
	wantNFO := filepath.Join(nfsRoot, "movies", "The Matrix (1999)", "The Matrix (1999).nfo")
	gotISO, err := os.ReadFile(wantISO)
	if err != nil || string(gotISO) != string(isoContent) {
		t.Fatalf("final ISO missing or corrupted at %s: %v", wantISO, err)
	}
	if _, err := os.Stat(wantNFO); err != nil {
		t.Fatalf("final NFO missing at %s: %v", wantNFO, err)
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("expected scratch jobDir %s to be deleted after successful transfer, err=%v", jobDir, err)
	}
}

func TestTargetCollisionRoutesToUnidentifiedWithoutOverwriting(t *testing.T) {
	ctx := context.Background()
	scratchDir := t.TempDir()
	nfsRoot := t.TempDir()

	srv := newMockTMDBServer(t)
	defer srv.Close()

	tmdb := metadata.NewTMDBClient("0123456789abcdef0123456789abcdef")
	tmdb.BaseURL = srv.URL + "/3"

	// Pre-populate an existing movie ISO on the NAS to trigger target_already_exists collision
	existingMovieDir := filepath.Join(nfsRoot, "movies", "The Matrix (1999)")
	if err := os.MkdirAll(existingMovieDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existingISOPath := filepath.Join(existingMovieDir, "The Matrix (1999).iso")
	if err := os.WriteFile(existingISOPath, []byte("original-disc-1-iso"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Second disc with same label/movie inserted
	jobDir := filepath.Join(scratchDir, "job_20261018T160000Z")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	isoPath := filepath.Join(jobDir, "disc.iso")
	if err := os.WriteFile(isoPath, []byte("second-disc-iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := &ripper.Job{
		JobID:         "job_20261018T160000Z",
		StartedAt:     "2026-10-18T16:00:00Z",
		State:         ripper.StateISOFinalized,
		Stage:         ripper.Stage1GenISOImage,
		RawLabel:      "THE_MATRIX_16X9",
		TracksSeconds: []float64{8160.0},
		JobDir:        jobDir,
		ISOPath:       isoPath,
	}

	writer := NewWriter(nfsRoot, 1000, 1000, tmdb)
	writer.Logger = log.New(io.Discard, "", 0)

	outcome, err := writer.ProcessJobOnce(ctx, job)
	if err != nil {
		t.Fatalf("ProcessJobOnce on collision failed: %v", err)
	}
	if outcome.AutoFiled || outcome.Reason != metadata.ReasonTargetAlreadyExists {
		t.Fatalf("expected routing to /unidentified with reason=%q, got %+v", metadata.ReasonTargetAlreadyExists, outcome)
	}

	// Verify original movie ISO was NOT overwritten
	origBytes, err := os.ReadFile(existingISOPath)
	if err != nil || string(origBytes) != "original-disc-1-iso" {
		t.Fatalf("existing movie ISO was overwritten or unreadable: %q, err=%v", string(origBytes), err)
	}

	// Verify .probe.json and .iso in /unidentified
	sidecar, err := metadata.ReadProbeSidecar(outcome.ProbeJSONPath)
	if err != nil {
		t.Fatalf("ReadProbeSidecar failed: %v", err)
	}
	if sidecar.Reason != metadata.ReasonTargetAlreadyExists || len(sidecar.TMDBCandidates) == 0 || sidecar.TMDBCandidates[0].ID != 603 {
		t.Fatalf("unexpected sidecar contents on collision: %+v", sidecar)
	}
}

func TestTVSeriesAndLocalISOReprobeWritesProbeJSON(t *testing.T) {
	ctx := context.Background()
	scratchDir := t.TempDir()
	nfsRoot := t.TempDir()

	srv := newMockTMDBServer(t)
	defer srv.Close()

	tmdb := metadata.NewTMDBClient("0123456789abcdef0123456789abcdef")
	tmdb.BaseURL = srv.URL + "/3"

	jobDir := filepath.Join(scratchDir, "job_20261018T163000Z")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	isoPath := filepath.Join(jobDir, "disc.iso")
	if err := os.WriteFile(isoPath, []byte("tv-series-iso"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate job whose initial /dev/sr0 probe returned empty tracks and "unknown" label
	job := &ripper.Job{
		JobID:         "job_20261018T163000Z",
		StartedAt:     "2026-10-18T16:30:00Z",
		State:         ripper.StateISOFinalized,
		Stage:         ripper.Stage2DDRescue,
		RawLabel:      "unknown",
		TracksSeconds: []float64{},
		JobDir:        jobDir,
		ISOPath:       isoPath,
	}
	_ = ripper.WriteJobFile(jobDir, job)

	writer := NewWriter(nfsRoot, 1000, 1000, tmdb)
	writer.Logger = log.New(io.Discard, "", 0)
	writer.RunCommand = func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		switch name {
		case "blkid":
			return "unknown\n", nil
		case "lsdvd":
			return `<lsdvd><title>OFFICE_S2_D1</title><track><ix>1</ix><length>1324.5</length></track><track><ix>2</ix><length>1318.2</length></track><track><ix>3</ix><length>1340.0</length></track></lsdvd>`, nil
		default:
			return "", nil
		}
	}

	outcome, err := writer.ProcessJobOnce(ctx, job)
	if err != nil {
		t.Fatalf("ProcessJobOnce failed: %v", err)
	}
	if outcome.AutoFiled || outcome.Classification != metadata.MediaTypeTVSeries || outcome.Reason != metadata.ReasonTVSeriesConfirmation {
		t.Fatalf("unexpected TV series outcome: %+v", outcome)
	}

	sidecar, err := metadata.ReadProbeSidecar(outcome.ProbeJSONPath)
	if err != nil {
		t.Fatalf("ReadProbeSidecar failed: %v", err)
	}
	if sidecar.RawLabel != "OFFICE_S2_D1" || sidecar.Hints.Season != 2 || sidecar.Hints.Disc != 1 || sidecar.Hints.EstimatedEpisodes != 3 {
		t.Fatalf("unexpected sidecar metadata after re-probe: %+v", sidecar)
	}
}

func TestTransientNFSFailureCleansPartialPreservesScratchAndRetriesWithBackoff(t *testing.T) {
	ctx := context.Background()
	scratchDir := t.TempDir()
	nfsRoot := t.TempDir()

	srv := newMockTMDBServer(t)
	defer srv.Close()

	tmdb := metadata.NewTMDBClient("0123456789abcdef0123456789abcdef")
	tmdb.BaseURL = srv.URL + "/3"

	jobDir := filepath.Join(scratchDir, "job_20261018T170000Z")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	isoPath := filepath.Join(jobDir, "disc.iso")
	if err := os.WriteFile(isoPath, []byte("retry-iso-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := &ripper.Job{
		JobID:         "job_20261018T170000Z",
		StartedAt:     "2026-10-18T17:00:00Z",
		State:         ripper.StateISOFinalized,
		Stage:         ripper.Stage1GenISOImage,
		RawLabel:      "THE_MATRIX_16X9",
		TracksSeconds: []float64{8160.0},
		JobDir:        jobDir,
		ISOPath:       isoPath,
	}

	var copyAttempts int32
	var retryEvents []TransferRetryEvent
	var observedRetryStatus bool

	writer := NewWriter(nfsRoot, 1000, 1000, tmdb)
	writer.Logger = log.New(io.Discard, "", 0)
	writer.RetryBackoffs = []time.Duration{5 * time.Millisecond, 10 * time.Millisecond}
	writer.CopyHook = func(dst io.Writer, src io.Reader, buf []byte) (int64, error) {
		n := atomic.AddInt32(&copyAttempts, 1)
		if n < 3 {
			// Write partial bytes then simulate transient NFS ENOSPC / network error
			_, _ = dst.Write([]byte("incomplete"))
			return 10, errors.New("simulated transient NFS write error")
		}
		return io.CopyBuffer(dst, src, buf)
	}
	writer.OnTransferRetry = func(ev TransferRetryEvent) {
		if writer.Status() == StatusNFSTransferPendingRetry {
			observedRetryStatus = true
		}
		// Verify incomplete .partial file was cleaned up and scratch disc.iso is still preserved
		partialPath := filepath.Join(nfsRoot, "movies", "The Matrix (1999)", ".The Matrix (1999).iso.partial")
		if _, err := os.Stat(partialPath); !os.IsNotExist(err) {
			t.Errorf("expected incomplete partial ISO %s to be removed on failure, err=%v", partialPath, err)
		}
		if _, err := os.Stat(isoPath); err != nil {
			t.Errorf("expected local scratch ISO %s to be preserved during retry, err=%v", isoPath, err)
		}
		retryEvents = append(retryEvents, ev)
	}

	outcome, err := writer.ProcessJobWithRetry(ctx, job)
	if err != nil {
		t.Fatalf("ProcessJobWithRetry failed: %v", err)
	}
	if !outcome.AutoFiled || atomic.LoadInt32(&copyAttempts) != 3 {
		t.Fatalf("expected success on 3rd attempt, got attempts=%d outcome=%+v", copyAttempts, outcome)
	}
	if !observedRetryStatus || len(retryEvents) != 2 {
		t.Fatalf("expected 2 retry events in %q status, got %d", StatusNFSTransferPendingRetry, len(retryEvents))
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("expected scratch jobDir to be deleted after retry succeeded, err=%v", err)
	}
}

func TestZeroCopyTaggingFromUnidentifiedToMovieAndTV(t *testing.T) {
	nfsRoot := t.TempDir()
	unidDir := filepath.Join(nfsRoot, "unidentified")
	if err := os.MkdirAll(unidDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writer := NewWriter(nfsRoot, 1000, 1000, nil)
	writer.Logger = log.New(io.Discard, "", 0)

	// 1. Tag unidentified ISO as Movie with Edition suffix ("Disc 2")
	unidMovieISO := filepath.Join(unidDir, "MATRIX_D2_20261018T153000Z.iso")
	unidMovieProbe := filepath.Join(unidDir, "MATRIX_D2_20261018T153000Z.probe.json")
	_ = os.WriteFile(unidMovieISO, []byte("matrix-disc2-iso"), 0o644)
	_ = os.WriteFile(unidMovieProbe, []byte("{}"), 0o644)

	finalMovieISO, err := writer.TagUnidentifiedAsMovie("MATRIX_D2_20261018T153000Z.iso", &metadata.TMDBMovieDetails{
		ID:          603,
		Title:       "The Matrix",
		ReleaseDate: "1999-03-30",
		Runtime:     136,
	}, "Disc 2", false)
	if err != nil {
		t.Fatalf("TagUnidentifiedAsMovie failed: %v", err)
	}
	wantMovieISO := filepath.Join(nfsRoot, "movies", "The Matrix (1999)", "The Matrix (1999) - Disc 2.iso")
	if finalMovieISO != wantMovieISO {
		t.Fatalf("got finalMovieISO=%q, want %q", finalMovieISO, wantMovieISO)
	}
	nfoData, err := os.ReadFile(filepath.Join(nfsRoot, "movies", "The Matrix (1999)", "The Matrix (1999) - Disc 2.nfo"))
	if err != nil || !strings.Contains(string(nfoData), "<title>The Matrix (Disc 2)</title>") {
		t.Fatalf("expected movie .nfo with Disc 2 title, got %q (err=%v)", string(nfoData), err)
	}
	if _, err := os.Stat(unidMovieProbe); !os.IsNotExist(err) {
		t.Fatalf("expected .probe.json to be deleted after tagging")
	}

	// 2. Tag unidentified ISO as TV Show (creates tvshow.nfo + Season 02/The Office - S02E01E02.iso & .nfo)
	unidTVISO := filepath.Join(unidDir, "OFFICE_S2_D1_20261018T153000Z.iso")
	unidTVProbe := filepath.Join(unidDir, "OFFICE_S2_D1_20261018T153000Z.probe.json")
	_ = os.WriteFile(unidTVISO, []byte("office-s2-iso"), 0o644)
	_ = os.WriteFile(unidTVProbe, []byte("{}"), 0o644)

	finalTVISO, err := writer.TagUnidentifiedAsTVShow("OFFICE_S2_D1_20261018T153000Z.iso", &metadata.TMDBTVDetails{
		ID:           2316,
		Name:         "The Office",
		FirstAirDate: "2005-03-24",
	}, 2, []metadata.TMDBEpisode{
		{ID: 101, Name: "The Dundies", SeasonNumber: 2, EpisodeNumber: 1, AirDate: "2005-09-20", Runtime: 21},
		{ID: 102, Name: "Sexual Harassment", SeasonNumber: 2, EpisodeNumber: 2, AirDate: "2005-09-27", Runtime: 22},
	}, false)
	if err != nil {
		t.Fatalf("TagUnidentifiedAsTVShow failed: %v", err)
	}
	wantTVISO := filepath.Join(nfsRoot, "tv", "The Office (2005)", "Season 02", "The Office - S02E01E02.iso")
	if finalTVISO != wantTVISO {
		t.Fatalf("got finalTVISO=%q, want %q", finalTVISO, wantTVISO)
	}
	if _, err := os.Stat(filepath.Join(nfsRoot, "tv", "The Office (2005)", "tvshow.nfo")); err != nil {
		t.Fatalf("expected tvshow.nfo to be created: %v", err)
	}
	if _, err := os.Stat(unidTVProbe); !os.IsNotExist(err) {
		t.Fatalf("expected TV .probe.json to be deleted after tagging")
	}
}

func TestListAndDeleteUnidentified(t *testing.T) {
	nfsRoot := t.TempDir()
	unidDir := filepath.Join(nfsRoot, "unidentified")
	if err := os.MkdirAll(unidDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var nfsUserCalls int
	writer := NewWriter(nfsRoot, 1000, 1000, nil)
	writer.Logger = log.New(io.Discard, "", 0)
	writer.NFSUserRunner = func(puid, pgid int, fn func() error) error {
		nfsUserCalls++
		return fn()
	}

	sc1, _ := metadata.MarshalProbeSidecar(&metadata.ProbeSidecar{
		ISOFilename:    "DISC_A_20261018T140000Z.iso",
		ProbedAt:       "2026-10-18T14:00:00Z",
		RawLabel:       "DISC_A",
		Classification: metadata.MediaTypeMovie,
	})
	sc2, _ := metadata.MarshalProbeSidecar(&metadata.ProbeSidecar{
		ISOFilename:    "DISC_B_20261018T150000Z.iso",
		ProbedAt:       "2026-10-18T15:00:00Z",
		RawLabel:       "DISC_B",
		Classification: metadata.MediaTypeTVSeries,
	})
	_ = os.WriteFile(filepath.Join(unidDir, "DISC_A_20261018T140000Z.iso"), []byte("iso-a"), 0o644)
	_ = os.WriteFile(filepath.Join(unidDir, "DISC_A_20261018T140000Z.probe.json"), sc1, 0o644)
	_ = os.WriteFile(filepath.Join(unidDir, "DISC_B_20261018T150000Z.iso"), []byte("iso-b"), 0o644)
	_ = os.WriteFile(filepath.Join(unidDir, "DISC_B_20261018T150000Z.probe.json"), sc2, 0o644)

	items, err := writer.ListUnidentified()
	if err != nil {
		t.Fatalf("ListUnidentified failed: %v", err)
	}
	if len(items) != 2 || items[0].RawLabel != "DISC_B" || items[1].RawLabel != "DISC_A" {
		t.Fatalf("expected 2 sorted items (DISC_B then DISC_A), got %+v", items)
	}

	sidecarA, err := writer.ReadUnidentifiedSidecar("DISC_A_20261018T140000Z.iso")
	if err != nil || sidecarA.RawLabel != "DISC_A" {
		t.Fatalf("ReadUnidentifiedSidecar failed: %+v (err=%v)", sidecarA, err)
	}

	sizeA, err := writer.UnidentifiedISOSize("DISC_A_20261018T140000Z.iso")
	if err != nil || sizeA != 5 {
		t.Fatalf("UnidentifiedISOSize failed: got %d, want 5 (err=%v)", sizeA, err)
	}

	if err := writer.DeleteUnidentified("DISC_A_20261018T140000Z.iso"); err != nil {
		t.Fatalf("DeleteUnidentified failed: %v", err)
	}
	itemsAfter, err := writer.ListUnidentified()
	if err != nil || len(itemsAfter) != 1 || itemsAfter[0].RawLabel != "DISC_B" {
		t.Fatalf("expected 1 remaining item DISC_B after delete, got %+v (err=%v)", itemsAfter, err)
	}
	if nfsUserCalls != 5 {
		t.Fatalf("expected 5 NFSUserRunner calls, got %d", nfsUserCalls)
	}
}

func newMockTMDBServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/3/search/movie", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []metadata.TMDBMovieSearchResult{
				{ID: 603, Title: "The Matrix", OriginalTitle: "The Matrix", ReleaseDate: "1999-03-30", Overview: "Hacker story", PosterPath: "/matrix.jpg"},
			},
		})
	})
	mux.HandleFunc("/3/movie/603", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(metadata.TMDBMovieDetails{
			ID:            603,
			IMDbID:        "tt0133093",
			Title:         "The Matrix",
			OriginalTitle: "The Matrix",
			ReleaseDate:   "1999-03-30",
			Runtime:       136,
			VoteAverage:   8.2,
			VoteCount:     24500,
			PosterPath:    "/matrix.jpg",
		})
	})
	mux.HandleFunc("/3/search/tv", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []metadata.TMDBTVSearchResult{
				{ID: 2316, Name: "The Office", FirstAirDate: "2005-03-24", Overview: "Dunder Mifflin", PosterPath: "/office.jpg"},
			},
		})
	})
	return httptest.NewServer(mux)
}
