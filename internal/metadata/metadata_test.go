package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseLSDVDXMLSanitizesBareAmpersandsAndInvalidUTF8(t *testing.T) {
	// Include libdvdcss stderr prefix, invalid UTF-8 byte (0xff), bare '&' in <title> and <provider_id>,
	// and a valid XML entity (&amp;) to verify all are handled cleanly.
	raw := append([]byte("libdvdcss debug: opening target `/dev/sr0'\n"),
		[]byte(`<?xml version="1.0" encoding="UTF-8"?>
<lsdvd>
  <device>/dev/sr0</device>
  <title>LAW_&_ORDER_S1`+"\xff"+`</title>
  <vmg_id>DVDVIDEO-VMG</vmg_id>
  <provider_id>UNIVERSAL &amp; P&S</provider_id>
  <track>
    <ix>1</ix>
    <length>2712.400</length>
    <vts_id>DVDVIDEO-VTS</vts_id>
  </track>
  <track>
    <ix>2</ix>
    <length>2698.120</length>
    <vts_id>DVDVIDEO-VTS</vts_id>
  </track>
  <longest_track>1</longest_track>
</lsdvd>`)...)

	parsed, err := ParseLSDVDXML(raw)
	if err != nil {
		t.Fatalf("ParseLSDVDXML failed: %v", err)
	}
	if parsed.Title != "LAW_&_ORDER_S1" {
		t.Errorf("parsed.Title = %q, want %q", parsed.Title, "LAW_&_ORDER_S1")
	}
	if parsed.ProviderID != "UNIVERSAL & P&S" {
		t.Errorf("parsed.ProviderID = %q, want %q", parsed.ProviderID, "UNIVERSAL & P&S")
	}
	if len(parsed.Tracks) != 2 || parsed.Tracks[0].Length != 2712.4 || parsed.Tracks[1].Length != 2698.12 {
		t.Fatalf("unexpected parsed tracks: %+v", parsed.Tracks)
	}
}

func TestProbeTargetBlkidPrimaryAndLSDVDFallback(t *testing.T) {
	ctx := context.Background()

	// Case 1: blkid succeeds -> primary label comes from blkid
	res1, err := ProbeTarget(ctx, func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		switch name {
		case "blkid":
			return "THE_MATRIX_16X9\n", nil
		case "lsdvd":
			return `<lsdvd><title>FALLBACK_TITLE</title><provider_id>WARNER_HOME_VIDEO</provider_id><track><ix>1</ix><length>8160.0</length></track></lsdvd>`, nil
		default:
			return "", nil
		}
	}, nil, "/dev/sr0")
	if err != nil {
		t.Fatalf("ProbeTarget Case 1 error: %v", err)
	}
	if res1.RawLabel != "THE_MATRIX_16X9" {
		t.Errorf("got RawLabel=%q, want THE_MATRIX_16X9", res1.RawLabel)
	}
	if len(res1.TracksSeconds) != 1 || res1.TracksSeconds[0] != 8160.0 {
		t.Errorf("unexpected TracksSeconds: %v", res1.TracksSeconds)
	}

	// Case 2: blkid returns "unknown" -> falls back to lsdvd <title> (never <provider_id>)
	res2, err := ProbeTarget(ctx, func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		switch name {
		case "blkid":
			return "unknown\n", nil
		case "lsdvd":
			return `<lsdvd><title>OFFICE_S2_D1</title><provider_id>UNIVERSAL_STUDIOS</provider_id><track><ix>1</ix><length>1320.5</length></track></lsdvd>`, nil
		default:
			return "", nil
		}
	}, nil, "/dev/sr0")
	if err != nil {
		t.Fatalf("ProbeTarget Case 2 error: %v", err)
	}
	if res2.RawLabel != "OFFICE_S2_D1" {
		t.Errorf("got RawLabel=%q, want OFFICE_S2_D1", res2.RawLabel)
	}
}

func TestClassifyTracksHeuristics(t *testing.T) {
	// 1. TV DVD with 4 episodes (22m each), duplicate PGCs (tracks 5-8 identical to 1-4),
	// short bumpers (< 18m), and an 88m composite "Play All" track.
	tvTracks := []float64{
		45.0, 120.5, // Menu bumpers (< 18m)
		1324.500, 1318.200, 1340.000, 1299.800, // Episodes 1-4
		1324.520, 1318.180, 1340.010, 1299.840, // Duplicate PGCs (Δ < 0.1s)
		5282.500, // Composite "Play All" track (sum = 5282.5s, |L_long - S| <= 300s)
	}
	tvRes := ClassifyTracks(tvTracks, "OFFICE_S2_D1")
	if tvRes.MediaType != MediaTypeTVSeries {
		t.Fatalf("expected TV_SERIES, got %s", tvRes.MediaType)
	}
	if tvRes.EstimatedEpisodes != 4 {
		t.Fatalf("expected EstimatedEpisodes=4, got %d", tvRes.EstimatedEpisodes)
	}
	if len(tvRes.PlayAllTracks) != 1 || tvRes.PlayAllTracks[0] != 5282.5 {
		t.Fatalf("expected PlayAllTracks=[5282.5], got %v", tvRes.PlayAllTracks)
	}

	// 2. Movie disc with a 110m feature (6600s) + two bonus featurettes in 18-45m (22m and 42m)
	// and no TV tokens in label -> MOVIE takes precedence (METADATA.md Section 2.4.1)
	movieWithExtras := []float64{6600.0, 1320.0, 2520.0}
	movieRes := ClassifyTracks(movieWithExtras, "THE_MATRIX_SE")
	if movieRes.MediaType != MediaTypeMovie {
		t.Fatalf("expected MOVIE precedence over 2 bonus featurettes, got %s", movieRes.MediaType)
	}

	// 3. Same tracks when volume label explicitly contains a TV token (S01) -> TV_SERIES
	tvOverrideRes := ClassifyTracks(movieWithExtras, "SHERLOCK_S01_D1")
	if tvOverrideRes.MediaType != MediaTypeTVSeries {
		t.Fatalf("expected TV_SERIES when label contains S01, got %s", tvOverrideRes.MediaType)
	}

	// 4. Multi-cut Movie DVD (Theatrical 115m + Director's Cut 132m) -> MOVIE
	multiCutRes := ClassifyTracks([]float64{6900.0, 7920.0}, "ALIENS_SE")
	if multiCutRes.MediaType != MediaTypeMovie {
		t.Fatalf("expected MOVIE for multi-cut disc, got %s", multiCutRes.MediaType)
	}

	// 5. Bonus disc with only short (< 18m) clips -> UNIDENTIFIED
	unidRes := ClassifyTracks([]float64{180.0, 420.0, 950.0}, "BONUS_DISC")
	if unidRes.MediaType != MediaTypeUnidentified {
		t.Fatalf("expected UNIDENTIFIED for short clips disc, got %s", unidRes.MediaType)
	}
}

func TestSanitizeLabelAndBlocklist(t *testing.T) {
	cases := []struct {
		raw        string
		wantQuery  string
		wantGen    bool
		wantSeason int
		wantDisc   int
		wantYear   int
	}{
		{"THE_MATRIX_16X9", "The Matrix", false, 0, 0, 0},
		{"JURASSIC_PARK_1993_WS_NTSC", "Jurassic Park", false, 0, 0, 1993},
		{"OFFICE_S2_D1", "Office", false, 2, 1, 0},
		{"THE_OFFICE_SEASON_03_DISC_2_PAL", "The Office", false, 3, 2, 0},
		{"1917_WS_NTSC", "1917", false, 0, 0, 0},
		{"2012_2009_SPECIAL_EDITION", "2012", false, 0, 0, 2009},
		{"DVD_VIDEO", "Dvd Video", true, 0, 0, 0},
		{"DISC_1", "", true, 0, 1, 0},
		{"LOGICAL_VOLUME_ID", "Logical Volume Id", true, 0, 0, 0},
		{"UNKNOWN", "Unknown", true, 0, 0, 0},
		{"A", "A", true, 0, 0, 0},
	}

	for _, tc := range cases {
		got := SanitizeLabel(tc.raw)
		if got.IsGeneric != tc.wantGen {
			t.Errorf("SanitizeLabel(%q).IsGeneric = %v, want %v", tc.raw, got.IsGeneric, tc.wantGen)
		}
		if !tc.wantGen && got.Query != tc.wantQuery {
			t.Errorf("SanitizeLabel(%q).Query = %q, want %q", tc.raw, got.Query, tc.wantQuery)
		}
		if got.Hints.Season != tc.wantSeason || got.Hints.Disc != tc.wantDisc || got.Hints.Year != tc.wantYear {
			t.Errorf("SanitizeLabel(%q).Hints = %+v, want S=%d D=%d Y=%d",
				tc.raw, got.Hints, tc.wantSeason, tc.wantDisc, tc.wantYear)
		}
	}
}

func TestTMDBAuthenticationAndRoutingPolicies(t *testing.T) {
	ctx := context.Background()

	var sawBearerHeader atomic.Bool
	var sawAPIKeyParam atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/3/search/movie", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			sawBearerHeader.Store(true)
		}
		if r.URL.Query().Get("api_key") != "" {
			sawAPIKeyParam.Store(true)
		}

		q := r.URL.Query().Get("query")
		switch q {
		case "The Matrix":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"results": []TMDBMovieSearchResult{
					{ID: 603, Title: "The Matrix", OriginalTitle: "The Matrix", ReleaseDate: "1999-03-30", Overview: "Hacker story", PosterPath: "/matrix.jpg"},
					{ID: 604, Title: "The Matrix Reloaded", OriginalTitle: "The Matrix Reloaded", ReleaseDate: "2003-05-15"},
				},
			})
		case "Total Recall":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"results": []TMDBMovieSearchResult{
					{ID: 861, Title: "Total Recall", OriginalTitle: "Total Recall", ReleaseDate: "1990-06-01"},
					{ID: 64635, Title: "Total Recall", OriginalTitle: "Total Recall", ReleaseDate: "2012-08-02"},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []TMDBMovieSearchResult{}})
		}
	})

	mux.HandleFunc("/3/movie/603", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(TMDBMovieDetails{
			ID:            603,
			IMDbID:        "tt0133093",
			Title:         "The Matrix",
			OriginalTitle: "The Matrix",
			ReleaseDate:   "1999-03-30",
			Runtime:       136,
		})
	})
	mux.HandleFunc("/3/movie/861", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(TMDBMovieDetails{
			ID:          861,
			Title:       "Total Recall",
			ReleaseDate: "1990-06-01",
			Runtime:     113,
		})
	})
	mux.HandleFunc("/3/movie/64635", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(TMDBMovieDetails{
			ID:          64635,
			Title:       "Total Recall",
			ReleaseDate: "2012-08-02",
			Runtime:     118,
		})
	})

	mux.HandleFunc("/3/search/tv", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []TMDBTVSearchResult{
				{ID: 2316, Name: "The Office", FirstAirDate: "2005-03-24", Overview: "Dunder Mifflin", PosterPath: "/office_us.jpg"},
				{ID: 2996, Name: "The Office", FirstAirDate: "2001-07-09", Overview: "Slough", PosterPath: "/office_uk.jpg"},
			},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 1. High-Confidence Movie Match with v3 API key (?api_key=)
	v3Client := NewTMDBClient("0123456789abcdef0123456789abcdef")
	v3Client.BaseURL = srv.URL + "/3"
	v3Client.Sleep = func(ctx context.Context, d time.Duration) error { return nil }

	matrixDec := EvaluateDisc(ctx, "THE_MATRIX_16X9", []float64{8160.0}, v3Client)
	if !sawAPIKeyParam.Load() {
		t.Fatalf("expected v3 API key to be passed as ?api_key= query param")
	}
	if !matrixDec.AutoFileMovie || matrixDec.MatchedMovie == nil || matrixDec.MatchedMovie.ID != 603 {
		t.Fatalf("expected high-confidence auto-file for The Matrix (603), got %+v", matrixDec)
	}

	// 2. High-Confidence Movie check with v4 JWT Bearer token (Authorization: Bearer eyJ...)
	v4Client := NewTMDBClient("eyJhbGciOiJIUzI1NiJ9.eyJhdWQiOiJ0ZXN0In0.signature")
	v4Client.BaseURL = srv.URL + "/3"
	v4Client.Sleep = func(ctx context.Context, d time.Duration) error { return nil }

	// Competing same-title remake without year in volume label -> routes to /unidentified (ambiguous_tmdb_match)
	remakeAmbig := EvaluateDisc(ctx, "TOTAL_RECALL_WS", []float64{115 * 60.0}, v4Client)
	if !sawBearerHeader.Load() {
		t.Fatalf("expected v4 JWT token to be passed via Authorization: Bearer header")
	}
	if remakeAmbig.AutoFileMovie || remakeAmbig.Reason != ReasonAmbiguousMatch {
		t.Fatalf("expected competing remake without year to be ambiguous, got %+v", remakeAmbig)
	}

	// Competing remake disambiguated by year in volume label -> AutoFileMovie == true!
	remakeDisambig := EvaluateDisc(ctx, "TOTAL_RECALL_1990_WS", []float64{115 * 60.0}, v4Client)
	if !remakeDisambig.AutoFileMovie || remakeDisambig.MatchedMovie == nil || remakeDisambig.MatchedMovie.ID != 861 {
		t.Fatalf("expected TOTAL_RECALL_1990_WS to auto-match 1990 version (861), got %+v", remakeDisambig)
	}

	// 3. TV Series disc -> always routes to /unidentified with pre-populated TV candidates & episode count
	tvTracks := []float64{1324.5, 1318.2, 1340.0, 1299.8, 1312.4, 1335.1}
	tvDec := EvaluateDisc(ctx, "OFFICE_S2_D1", tvTracks, v4Client)
	if tvDec.AutoFileMovie || tvDec.Classification != MediaTypeTVSeries || tvDec.Reason != ReasonTVSeriesConfirmation {
		t.Fatalf("expected TV series routing to /unidentified, got %+v", tvDec)
	}
	if tvDec.Hints.Season != 2 || tvDec.Hints.Disc != 1 || tvDec.Hints.EstimatedEpisodes != 6 {
		t.Fatalf("unexpected TV hints: %+v", tvDec.Hints)
	}
	if len(tvDec.Candidates) != 2 || tvDec.Candidates[0].ID != 2316 {
		t.Fatalf("expected 2 pre-populated TV candidates, got %+v", tvDec.Candidates)
	}
}

func TestTMDBOfflineFallbackAfterThreeRetries(t *testing.T) {
	ctx := context.Background()

	var attempts int32
	var sleepDurations []time.Duration

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status_message":"service unavailable"}`))
	}))
	defer srv.Close()

	client := NewTMDBClient("0123456789abcdef0123456789abcdef")
	client.BaseURL = srv.URL + "/3"
	client.Sleep = func(ctx context.Context, d time.Duration) error {
		sleepDurations = append(sleepDurations, d)
		return nil
	}

	dec := EvaluateDisc(ctx, "THE_MATRIX_16X9", []float64{8160.0}, client)
	if dec.AutoFileMovie || dec.Reason != ReasonTMDBLookupError {
		t.Fatalf("expected offline fallback with reason=%q, got %+v", ReasonTMDBLookupError, dec)
	}
	if got := atomic.LoadInt32(&attempts); got != 4 {
		t.Fatalf("expected 1 initial + 3 retry attempts (4 total), got %d", got)
	}
	if len(sleepDurations) != 3 ||
		sleepDurations[0] != 1*time.Second ||
		sleepDurations[1] != 2*time.Second ||
		sleepDurations[2] != 4*time.Second {
		t.Fatalf("expected retry backoffs [1s, 2s, 4s], got %v", sleepDurations)
	}
}

func TestKodiNFOGenerationAndFilesystemSanitization(t *testing.T) {
	if got := SanitizeFilesystemName(`Star Wars: Episode IV - A New Hope? *Special*`); got != "Star Wars - Episode IV - A New Hope Special" {
		t.Fatalf("unexpected sanitized name: %q", got)
	}

	movie := &TMDBMovieDetails{
		ID:            603,
		IMDbID:        "tt0133093",
		Title:         "The Matrix",
		OriginalTitle: "The Matrix",
		ReleaseDate:   "1999-03-30",
		Runtime:       136,
		Tagline:       "Welcome to the Real World.",
		Overview:      "Set in the 22nd century...",
		VoteAverage:   8.2,
		VoteCount:     24500,
		PosterPath:    "/f89U3ADr1oiB1s9GkdPOEpXUk5H.jpg",
		BackdropPath:  "/fNG7i7RqMErkcqhohV2a6cV1Ehy.jpg",
		Genres:        []TMDBNamedItem{{Name: "Action"}, {Name: "Science Fiction"}},
		ProductionCompanies: []TMDBNamedItem{
			{Name: "Warner Bros. Pictures"},
		},
		Credits: TMDBCredits{
			Cast: []TMDBCastMember{{Name: "Keanu Reeves", Character: "Neo", Order: 0}},
			Crew: []TMDBCrewMember{
				{Name: "Lilly Wachowski", Job: "Director"},
				{Name: "Lana Wachowski", Job: "Director"},
			},
		},
	}
	movie.ReleaseDates.Results = append(movie.ReleaseDates.Results, struct {
		ISO3166_1    string `json:"iso_3166_1"`
		ReleaseDates []struct {
			Certification string `json:"certification"`
			Type          int    `json:"type"`
		} `json:"release_dates"`
	}{
		ISO3166_1: "US",
		ReleaseDates: []struct {
			Certification string `json:"certification"`
			Type          int    `json:"type"`
		}{
			{Certification: "R", Type: 3},
		},
	})

	nfoBytes, err := GenerateMovieNFO(movie, "")
	if err != nil {
		t.Fatalf("GenerateMovieNFO failed: %v", err)
	}
	nfoStr := string(nfoBytes)
	for _, wantSub := range []string{
		`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`,
		`<title>The Matrix</title>`,
		`<year>1999</year>`,
		`<runtime>136</runtime>`,
		`<mpaa>Rated R</mpaa>`,
		`<uniqueid type="tmdb" default="true">603</uniqueid>`,
		`<uniqueid type="imdb">tt0133093</uniqueid>`,
		`<director>Lilly Wachowski</director>`,
		`<thumb aspect="poster">https://image.tmdb.org/t/p/original/f89U3ADr1oiB1s9GkdPOEpXUk5H.jpg</thumb>`,
	} {
		if !strings.Contains(nfoStr, wantSub) {
			t.Errorf("movie .nfo missing %q:\n%s", wantSub, nfoStr)
		}
	}

	// Multi-episode NFO without outer wrapper tag
	epNFO, err := GenerateMultiEpisodeNFO("The Office", []TMDBEpisode{
		{ID: 170031, Name: "Pilot", SeasonNumber: 1, EpisodeNumber: 1, AirDate: "2005-03-24", Runtime: 23},
		{ID: 170032, Name: "Diversity Day", SeasonNumber: 1, EpisodeNumber: 2, AirDate: "2005-03-29", Runtime: 22},
	})
	if err != nil {
		t.Fatalf("GenerateMultiEpisodeNFO failed: %v", err)
	}
	epNFOStr := string(epNFO)
	if strings.Count(epNFOStr, "<episodedetails>") != 2 {
		t.Fatalf("expected 2 root-level <episodedetails> blocks, got:\n%s", epNFOStr)
	}
	if FormatMultiEpisodeBaseName("The Office", 1, []int{1, 2, 3, 4}) != "The Office - S01E01E02E03E04" {
		t.Fatalf("unexpected multi-episode base name")
	}
}

func TestProbeTargetReturnsLSDVDErrorOnFailure(t *testing.T) {
	ctx := context.Background()
	_, err := ProbeTarget(ctx, func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		if name == "lsdvd" {
			return "", errors.New("exit status 1")
		}
		return "", nil
	}, nil, "/dev/sr0")
	if err == nil {
		t.Fatalf("expected error when lsdvd fails")
	}
}
