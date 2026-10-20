package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brotherlogic/hometheatre/internal/drive"
	"github.com/brotherlogic/hometheatre/internal/history"
	"github.com/brotherlogic/hometheatre/internal/metadata"
	"github.com/brotherlogic/hometheatre/internal/nfs"
	"github.com/brotherlogic/hometheatre/internal/ripper"
	"github.com/brotherlogic/hometheatre/internal/webhook"
)

type stubDrivePoller struct {
	mu          sync.RWMutex
	driveStatus int
	latched     bool
	label       string
}

func (s *stubDrivePoller) Status() (int, bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.driveStatus, s.latched, s.label
}

func (s *stubDrivePoller) set(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.driveStatus = code
	s.label = drive.DriveStatusString(code)
}

type stubRipperWorker struct {
	mu       sync.RWMutex
	status   string
	progress ripper.Progress
}

func (s *stubRipperWorker) Status() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

func (s *stubRipperWorker) Progress() ripper.Progress {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.progress
}

func (s *stubRipperWorker) set(st string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
	s.progress = ripper.Progress{}
}

func (s *stubRipperWorker) setWithProgress(st string, prog ripper.Progress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
	s.progress = prog
}

func TestLiveDriveAndNFSQueueStatusAndRetryButton(t *testing.T) {
	scratchDir := t.TempDir()
	nfsRoot := t.TempDir()

	poller := &stubDrivePoller{}
	poller.set(drive.CDS_NO_DISC)

	ripWorker := &stubRipperWorker{status: ripper.StatusIdle}
	nfsWriter := nfs.NewWriter(nfsRoot, 0, 0, nil)
	nfsWriter.Logger = log.New(io.Discard, "", 0)

	histStore, err := history.NewStore(scratchDir)
	if err != nil {
		t.Fatal(err)
	}
	logBuf := history.NewLogBuffer(50)
	_, _ = logBuf.Write([]byte("daemon started on /dev/sr0\n"))

	srv := NewServer(poller, ripWorker, nfsWriter, nil, histStore, logBuf, nil)
	srv.Logger = log.New(io.Discard, "", 0)
	srv.QueueCountFunc = func() int { return 2 }

	// Verify all required drive + ripper states render accurately in both HTML and /api/status
	cases := []struct {
		driveCode   int
		ripperState string
		wantLabel   string
		wantActive  bool
	}{
		{drive.CDS_NO_DISC, ripper.StatusIdle, "Empty", false},
		{drive.CDS_TRAY_OPEN, ripper.StatusIdle, "Tray Open", false},
		{drive.CDS_DRIVE_NOT_READY, ripper.StatusIdle, "Spinning Up / Reading TOC", false},
		{drive.CDS_DISC_OK, ripper.StatusWaitingForScratch, "Waiting for Scratch Space", false},
		{drive.CDS_DISC_OK, ripper.StatusRippingStage1DVDBackup, "Ripping Stage 1 (stage1_dvdbackup)", true},
		{drive.CDS_DISC_OK, ripper.StatusRippingStage2DDRescue, "Ripping Stage 2 (stage2_ddrescue)", true},
	}

	for _, tc := range cases {
		poller.set(tc.driveCode)
		ripWorker.set(tc.ripperState)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET / returned HTTP %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), tc.wantLabel) {
			t.Fatalf("expected HTML dashboard to contain %q, body did not match", tc.wantLabel)
		}

		apiReq := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		apiRec := httptest.NewRecorder()
		srv.ServeHTTP(apiRec, apiReq)
		var st StatusResponse
		if err := json.NewDecoder(apiRec.Body).Decode(&st); err != nil {
			t.Fatal(err)
		}
		if st.CombinedDriveStatus != tc.wantLabel || st.PendingTransfers != 2 || st.RipActive != tc.wantActive {
			t.Fatalf("got status %+v, want CombinedDriveStatus=%q PendingTransfers=2 RipActive=%v", st, tc.wantLabel, tc.wantActive)
		}
	}

	// Verify live ripping progress bar and /api/status byte/percentage fields
	const gib = 1024 * 1024 * 1024
	poller.set(drive.CDS_DISC_OK)
	ripWorker.setWithProgress(ripper.StatusRippingStage1DVDBackup, ripper.Progress{
		Active:     true,
		Stage:      ripper.Stage1DVDBackup,
		Label:      "THE_MATRIX_16X9",
		BytesDone:  2 * gib,
		BytesTotal: 4 * gib,
		Percent:    50.0,
	})

	progAPIReq := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	progAPIRec := httptest.NewRecorder()
	srv.ServeHTTP(progAPIRec, progAPIReq)
	var progSt StatusResponse
	if err := json.NewDecoder(progAPIRec.Body).Decode(&progSt); err != nil {
		t.Fatal(err)
	}
	if !progSt.RipActive || progSt.RipProgressPct != 50.0 || progSt.RipBytesDone != 2*gib || progSt.RipBytesTotal != 4*gib || progSt.RipLabel != "THE_MATRIX_16X9" {
		t.Fatalf("unexpected progress in /api/status: %+v", progSt)
	}

	progHTMLReq := httptest.NewRequest(http.MethodGet, "/", nil)
	progHTMLRec := httptest.NewRecorder()
	srv.ServeHTTP(progHTMLRec, progHTMLReq)
	progHTML := progHTMLRec.Body.String()
	for _, wantSnippet := range []string{
		`id="rip-progress-container"`,
		`THE_MATRIX_16X9`,
		`50.0% (2.00 GiB / 4.00 GiB)`,
		`width: 50.0%;`,
		`setInterval(pollStatus, 2000)`,
	} {
		if !strings.Contains(progHTML, wantSnippet) {
			t.Fatalf("expected dashboard HTML to contain %q, got:\n%s", wantSnippet, progHTML)
		}
	}

	// Test manual "Retry Transfer Now" action via POST /api/retry-transfer and POST /retry-transfer
	retryReq := httptest.NewRequest(http.MethodPost, "/api/retry-transfer", nil)
	retryRec := httptest.NewRecorder()
	srv.ServeHTTP(retryRec, retryReq)
	if retryRec.Code != http.StatusOK {
		t.Fatalf("POST /api/retry-transfer returned HTTP %d", retryRec.Code)
	}

	formRetryReq := httptest.NewRequest(http.MethodPost, "/retry-transfer", nil)
	formRetryRec := httptest.NewRecorder()
	srv.ServeHTTP(formRetryRec, formRetryReq)
	if formRetryRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /retry-transfer expected 303 redirect, got %d", formRetryRec.Code)
	}
}

func TestUnidentifiedQueueTVShowTaggingAndWebhook(t *testing.T) {
	scratchDir := t.TempDir()
	nfsRoot := t.TempDir()
	unidDir := filepath.Join(nfsRoot, "unidentified")
	if err := os.MkdirAll(unidDir, 0o755); err != nil {
		t.Fatal(err)
	}

	tmdbSrv := newMockWebUITMDBServer(t)
	defer tmdbSrv.Close()
	tmdb := metadata.NewTMDBClient("0123456789abcdef0123456789abcdef")
	tmdb.BaseURL = tmdbSrv.URL + "/3"

	var webhookMu sync.Mutex
	var webhooks []webhook.Payload
	whSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookMu.Lock()
		defer webhookMu.Unlock()
		var p webhook.Payload
		_ = json.NewDecoder(r.Body).Decode(&p)
		webhooks = append(webhooks, p)
		w.WriteHeader(http.StatusOK)
	}))
	defer whSrv.Close()

	notifier := webhook.NewNotifier(whSrv.URL)
	histStore, err := history.NewStore(scratchDir)
	if err != nil {
		t.Fatal(err)
	}

	// Seed an unidentified TV Series disc with 6 estimated episodes in /mnt/nfs/unidentified
	isoFilename := "OFFICE_S2_D1_20261018T153000Z.iso"
	isoPath := filepath.Join(unidDir, isoFilename)
	probePath := filepath.Join(unidDir, "OFFICE_S2_D1_20261018T153000Z.probe.json")
	if err := os.WriteFile(isoPath, []byte("office-s2-d1-iso-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	sidecar := &metadata.ProbeSidecar{
		ISOFilename:    isoFilename,
		ProbedAt:       "2026-10-18T15:30:00Z",
		RawLabel:       "OFFICE_S2_D1",
		SanitizedQuery: "Office",
		Classification: metadata.MediaTypeTVSeries,
		Reason:         metadata.ReasonTVSeriesConfirmation,
		Hints: metadata.LabelHints{
			Season:            2,
			Disc:              1,
			EstimatedEpisodes: 6,
		},
		TracksSeconds: []float64{1324.5, 1318.2, 1340.0, 1299.8, 1312.4, 1335.1},
		TMDBCandidates: []metadata.TMDBCandidate{
			{
				ID:         2316,
				MediaType:  "tv",
				Title:      "The Office",
				Year:       2005,
				Overview:   "Scranton branch of Dunder Mifflin.",
				PosterPath: "/office.jpg",
			},
		},
	}
	probeBytes, err := metadata.MarshalProbeSidecar(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(probePath, probeBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	_ = histStore.Record(history.Entry{
		JobID:          "job_20261018T153000Z",
		RawLabel:       "OFFICE_S2_D1",
		Classification: metadata.MediaTypeTVSeries,
		Status:         history.StatusUnidentified,
		Stage:          ripper.Stage1DVDBackup,
		ISOPath:        isoPath,
		Reason:         metadata.ReasonTVSeriesConfirmation,
	})

	nfsWriter := nfs.NewWriter(nfsRoot, 0, 0, tmdb)
	nfsWriter.Logger = log.New(io.Discard, "", 0)

	srv := NewServer(&stubDrivePoller{label: "Empty"}, &stubRipperWorker{status: "Idle"}, nfsWriter, tmdb, histStore, nil, notifier)
	srv.Logger = log.New(io.Discard, "", 0)

	// 1. Verify GET /unidentified pre-fills candidate (2316), Season 2, Episodes 1..6, and S02E01E02E03E04E05E06
	getReq := httptest.NewRequest(http.MethodGet, "/unidentified", nil)
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /unidentified returned HTTP %d", getRec.Code)
	}
	html := getRec.Body.String()
	for _, want := range []string{
		"OFFICE_S2_D1",
		"The Office",
		"S02E01E02E03E04E05E06",
		`value="2316"`,
		`value="6"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("expected /unidentified HTML to contain %q", want)
		}
	}

	// 2. Execute 1-click confirmation via form POST /unidentified/tag
	form := url.Values{
		"iso_filename":  {isoFilename},
		"action":        {"tag"},
		"media_type":    {"tv"},
		"tmdb_id":       {"2316"},
		"season":        {"2"},
		"start_episode": {"1"},
		"end_episode":   {"6"},
	}
	postReq := httptest.NewRequest(http.MethodPost, "/unidentified/tag", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postRec := httptest.NewRecorder()
	srv.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /unidentified/tag expected 303 redirect, got %d: %s", postRec.Code, postRec.Body.String())
	}

	notifier.Wait()

	// 3. Verify zero-copy rename to /mnt/nfs/tv/The Office (2005)/Season 02/The Office - S02E01E02E03E04E05E06.iso
	wantFinalISO := filepath.Join(nfsRoot, "tv", "The Office (2005)", "Season 02", "The Office - S02E01E02E03E04E05E06.iso")
	wantFinalNFO := filepath.Join(nfsRoot, "tv", "The Office (2005)", "Season 02", "The Office - S02E01E02E03E04E05E06.nfo")
	wantTVShowNFO := filepath.Join(nfsRoot, "tv", "The Office (2005)", "tvshow.nfo")

	if _, err := os.Stat(wantFinalISO); err != nil {
		t.Fatalf("expected final TV ISO at %s: %v", wantFinalISO, err)
	}
	if _, err := os.Stat(wantTVShowNFO); err != nil {
		t.Fatalf("expected tvshow.nfo at %s: %v", wantTVShowNFO, err)
	}
	nfoBytes, err := os.ReadFile(wantFinalNFO)
	if err != nil {
		t.Fatalf("expected multi-episode NFO at %s: %v", wantFinalNFO, err)
	}
	if strings.Count(string(nfoBytes), "<episodedetails>") != 6 {
		t.Fatalf("expected 6 <episodedetails> blocks in NFO, got:\n%s", string(nfoBytes))
	}
	if _, err := os.Stat(probePath); !os.IsNotExist(err) {
		t.Fatalf("expected .probe.json to be deleted after tagging")
	}

	// 4. Verify history.json and webhook notification
	entries := histStore.Entries()
	if len(entries) != 1 || entries[0].Status != history.StatusCompleted || entries[0].ISOPath != wantFinalISO {
		t.Fatalf("expected history entry to be updated to completed, got %+v", entries)
	}
	webhookMu.Lock()
	defer webhookMu.Unlock()
	if len(webhooks) != 1 || webhooks[0].Event != webhook.EventRipCompleted || webhooks[0].ISOPath != wantFinalISO {
		t.Fatalf("expected rip_completed webhook, got %+v", webhooks)
	}
}

func TestMultiDiscMovieCollisionResolutionOptions(t *testing.T) {
	scratchDir := t.TempDir()
	nfsRoot := t.TempDir()
	unidDir := filepath.Join(nfsRoot, "unidentified")
	movieDir := filepath.Join(nfsRoot, "movies", "The Matrix (1999)")
	if err := os.MkdirAll(unidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(movieDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Existing Disc 1 movie ISO
	existingISO := filepath.Join(movieDir, "The Matrix (1999).iso")
	if err := os.WriteFile(existingISO, []byte("matrix-disc-1-original"), 0o644); err != nil {
		t.Fatal(err)
	}

	tmdbSrv := newMockWebUITMDBServer(t)
	defer tmdbSrv.Close()
	tmdb := metadata.NewTMDBClient("0123456789abcdef0123456789abcdef")
	tmdb.BaseURL = tmdbSrv.URL + "/3"

	histStore, _ := history.NewStore(scratchDir)
	nfsWriter := nfs.NewWriter(nfsRoot, 0, 0, tmdb)
	nfsWriter.Logger = log.New(io.Discard, "", 0)
	srv := NewServer(&stubDrivePoller{label: "Empty"}, &stubRipperWorker{status: "Idle"}, nfsWriter, tmdb, histStore, nil, nil)
	srv.Logger = log.New(io.Discard, "", 0)

	seedCollisionDisc := func(name, content string) {
		_ = os.WriteFile(filepath.Join(unidDir, name+".iso"), []byte(content), 0o644)
		sc := &metadata.ProbeSidecar{
			ISOFilename:    name + ".iso",
			ProbedAt:       time.Now().UTC().Format(time.RFC3339),
			RawLabel:       "THE_MATRIX",
			SanitizedQuery: "The Matrix",
			Classification: metadata.MediaTypeMovie,
			Reason:         metadata.ReasonTargetAlreadyExists,
			TMDBCandidates: []metadata.TMDBCandidate{
				{ID: 603, MediaType: "movie", Title: "The Matrix", Year: 1999},
			},
		}
		b, _ := metadata.MarshalProbeSidecar(sc)
		_ = os.WriteFile(filepath.Join(unidDir, name+".probe.json"), b, 0o644)
	}

	// Option 1: Save with Edition / Disc Suffix ("Bonus Features")
	seedCollisionDisc("MATRIX_BONUS_1", "bonus-features-iso")
	body1, _ := json.Marshal(TagRequest{
		ISOFilename:   "MATRIX_BONUS_1.iso",
		Action:        "tag",
		MediaType:     "movie",
		TMDBID:        603,
		EditionSuffix: "Bonus Features",
	})
	req1 := httptest.NewRequest(http.MethodPost, "/api/unidentified/tag", bytes.NewReader(body1))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	srv.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("Option 1 (Edition suffix) failed with HTTP %d: %s", rec1.Code, rec1.Body.String())
	}
	wantSuffixISO := filepath.Join(movieDir, "The Matrix (1999) - Bonus Features.iso")
	wantSuffixNFO := filepath.Join(movieDir, "The Matrix (1999) - Bonus Features.nfo")
	if _, err := os.Stat(wantSuffixISO); err != nil {
		t.Fatalf("expected suffixed ISO at %s: %v", wantSuffixISO, err)
	}
	nfoData, err := os.ReadFile(wantSuffixNFO)
	if err != nil || !strings.Contains(string(nfoData), "<title>The Matrix (Bonus Features)</title>") {
		t.Fatalf("expected NFO title with Bonus Features suffix, got %q", string(nfoData))
	}

	// Option 2: Replace Existing .iso (action="replace")
	seedCollisionDisc("MATRIX_REMASTER_2", "remastered-disc-1-iso")
	body2, _ := json.Marshal(TagRequest{
		ISOFilename: "MATRIX_REMASTER_2.iso",
		Action:      "replace",
		MediaType:   "movie",
		TMDBID:      603,
	})
	req2 := httptest.NewRequest(http.MethodPost, "/api/unidentified/tag", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("Option 2 (Replace existing) failed with HTTP %d: %s", rec2.Code, rec2.Body.String())
	}
	replacedBytes, err := os.ReadFile(existingISO)
	if err != nil || string(replacedBytes) != "remastered-disc-1-iso" {
		t.Fatalf("expected existing ISO to be replaced, got %q (err=%v)", string(replacedBytes), err)
	}

	// Option 3: Delete Unidentified .iso (action="delete")
	seedCollisionDisc("MATRIX_DUP_3", "duplicate-iso")
	body3, _ := json.Marshal(TagRequest{
		ISOFilename: "MATRIX_DUP_3.iso",
		Action:      "delete",
	})
	req3 := httptest.NewRequest(http.MethodPost, "/api/unidentified/delete", bytes.NewReader(body3))
	req3.Header.Set("Content-Type", "application/json")
	rec3 := httptest.NewRecorder()
	srv.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("Option 3 (Delete unidentified) failed with HTTP %d: %s", rec3.Code, rec3.Body.String())
	}
	if _, err := os.Stat(filepath.Join(unidDir, "MATRIX_DUP_3.iso")); !os.IsNotExist(err) {
		t.Fatalf("expected deleted ISO to be removed from /unidentified")
	}
	if _, err := os.Stat(filepath.Join(unidDir, "MATRIX_DUP_3.probe.json")); !os.IsNotExist(err) {
		t.Fatalf("expected deleted .probe.json to be removed from /unidentified")
	}
}

func TestAPITMDBSearchAndHistoryEndpoints(t *testing.T) {
	scratchDir := t.TempDir()
	nfsRoot := t.TempDir()

	tmdbSrv := newMockWebUITMDBServer(t)
	defer tmdbSrv.Close()
	tmdb := metadata.NewTMDBClient("0123456789abcdef0123456789abcdef")
	tmdb.BaseURL = tmdbSrv.URL + "/3"

	histStore, _ := history.NewStore(scratchDir)
	_ = histStore.Record(history.Entry{
		JobID:          "job_20261018T180000Z",
		RawLabel:       "THE_MATRIX_16X9",
		Title:          "The Matrix (1999)",
		Classification: metadata.MediaTypeMovie,
		Status:         history.StatusCompleted,
		Stage:          ripper.Stage2DDRescue,
		RescuedRatio:   0.985,
		ISOPath:        "/mnt/nfs/movies/The Matrix (1999)/The Matrix (1999).iso",
	})

	nfsWriter := nfs.NewWriter(nfsRoot, 0, 0, tmdb)
	nfsWriter.Logger = log.New(io.Discard, "", 0)
	srv := NewServer(&stubDrivePoller{label: "Empty"}, &stubRipperWorker{status: "Idle"}, nfsWriter, tmdb, histStore, nil, nil)
	srv.Logger = log.New(io.Discard, "", 0)

	// Verify /api/tmdb/search for movie and tv
	for _, mt := range []string{"movie", "tv"} {
		req := httptest.NewRequest(http.MethodGet, "/api/tmdb/search?q=test&type="+mt, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/tmdb/search?type=%s returned HTTP %d: %s", mt, rec.Code, rec.Body.String())
		}
		var out struct {
			Candidates []metadata.TMDBCandidate `json:"candidates"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil || len(out.Candidates) == 0 {
			t.Fatalf("expected non-empty TMDB search candidates for %s, got %+v (err=%v)", mt, out, err)
		}
	}

	// Verify /api/history
	histReq := httptest.NewRequest(http.MethodGet, "/api/history", nil)
	histRec := httptest.NewRecorder()
	srv.ServeHTTP(histRec, histReq)
	if histRec.Code != http.StatusOK {
		t.Fatalf("GET /api/history returned HTTP %d", histRec.Code)
	}
	if !strings.Contains(histRec.Body.String(), "The Matrix (1999)") {
		t.Fatalf("expected /api/history to include The Matrix (1999), got %s", histRec.Body.String())
	}
}

func newMockWebUITMDBServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/3/search/movie", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []metadata.TMDBMovieSearchResult{
				{ID: 603, Title: "The Matrix", ReleaseDate: "1999-03-30"},
			},
		})
	})
	mux.HandleFunc("/3/search/tv", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []metadata.TMDBTVSearchResult{
				{ID: 2316, Name: "The Office", FirstAirDate: "2005-03-24"},
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
		})
	})
	mux.HandleFunc("/3/tv/2316", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(metadata.TMDBTVDetails{
			ID:           2316,
			Name:         "The Office",
			OriginalName: "The Office",
			FirstAirDate: "2005-03-24",
			Overview:     "Scranton branch of Dunder Mifflin.",
		})
	})
	mux.HandleFunc("/3/tv/2316/season/2", func(w http.ResponseWriter, r *http.Request) {
		var eps []metadata.TMDBEpisode
		for i := 1; i <= 6; i++ {
			eps = append(eps, metadata.TMDBEpisode{
				ID:            2000 + i,
				Name:          "Season 2 Episode",
				SeasonNumber:  2,
				EpisodeNumber: i,
				AirDate:       "2005-09-20",
				Runtime:       22,
			})
		}
		_ = json.NewEncoder(w).Encode(metadata.TMDBSeasonDetails{
			ID:           302,
			SeasonNumber: 2,
			Name:         "Season 2",
			Episodes:     eps,
		})
	})
	return httptest.NewServer(mux)
}
