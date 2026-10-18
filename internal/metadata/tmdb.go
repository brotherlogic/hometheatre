package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	DefaultTMDBBaseURL = "https://api.themoviedb.org/3"
	TMDBImageBaseURL   = "https://image.tmdb.org/t/p/original"

	MaxMovieCandidatesToInspect = 5
	RuntimeToleranceMinutes     = 15.0

	ReasonTVSeriesConfirmation = "tv_series_requires_episode_confirmation"
	ReasonGenericLabel         = "generic_volume_label"
	ReasonTMDBLookupError      = "tmdb_lookup_error"
	ReasonAmbiguousMatch       = "ambiguous_tmdb_match"
	ReasonNoConfidentMatch     = "no_confident_tmdb_match"
	ReasonUnidentifiedMedia    = "unidentified_media_type"
	ReasonTargetAlreadyExists  = "target_already_exists"
)

// DefaultRetryBackoffs defines the 3 retry intervals (1s, 2s, 4s) per METADATA.md Section 3.4.2.
var DefaultRetryBackoffs = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
}

// TMDBCandidate represents a pre-populated candidate inside .probe.json (METADATA.md Section 4.1).
type TMDBCandidate struct {
	ID         int    `json:"id"`
	MediaType  string `json:"media_type"`
	Title      string `json:"title"`
	Year       int    `json:"year"`
	Overview   string `json:"overview"`
	PosterPath string `json:"poster_path"`
}

// TMDBMovieSearchResult represents an entry returned by GET /3/search/movie.
type TMDBMovieSearchResult struct {
	ID            int     `json:"id"`
	Title         string  `json:"title"`
	OriginalTitle string  `json:"original_title"`
	ReleaseDate   string  `json:"release_date"`
	Overview      string  `json:"overview"`
	PosterPath    string  `json:"poster_path"`
	BackdropPath  string  `json:"backdrop_path"`
	VoteAverage   float64 `json:"vote_average"`
	VoteCount     int     `json:"vote_count"`
}

// Year parses the 4-digit release year from ReleaseDate ("YYYY-MM-DD").
func (m TMDBMovieSearchResult) Year() int {
	return parseYearPrefix(m.ReleaseDate)
}

// TMDBMovieDetails represents the response from GET /3/movie/{movie_id}?append_to_response=credits,release_dates.
type TMDBMovieDetails struct {
	ID                  int                      `json:"id"`
	IMDbID              string                   `json:"imdb_id"`
	Title               string                   `json:"title"`
	OriginalTitle       string                   `json:"original_title"`
	ReleaseDate         string                   `json:"release_date"`
	Runtime             int                      `json:"runtime"`
	Tagline             string                   `json:"tagline"`
	Overview            string                   `json:"overview"`
	VoteAverage         float64                  `json:"vote_average"`
	VoteCount           int                      `json:"vote_count"`
	PosterPath          string                   `json:"poster_path"`
	BackdropPath        string                   `json:"backdrop_path"`
	Genres              []TMDBNamedItem          `json:"genres"`
	ProductionCompanies []TMDBNamedItem          `json:"production_companies"`
	Credits             TMDBCredits              `json:"credits"`
	ReleaseDates        TMDBReleaseDatesEnvelope `json:"release_dates"`
}

// Year parses the 4-digit release year from ReleaseDate ("YYYY-MM-DD").
func (m *TMDBMovieDetails) Year() int {
	if m == nil {
		return 0
	}
	return parseYearPrefix(m.ReleaseDate)
}

// USCertification extracts the US MPAA rating (formatted e.g. "Rated R") from release_dates.
func (m *TMDBMovieDetails) USCertification() string {
	if m == nil {
		return ""
	}
	for _, r := range m.ReleaseDates.Results {
		if !strings.EqualFold(r.ISO3166_1, "US") {
			continue
		}
		for _, rd := range r.ReleaseDates {
			cert := strings.TrimSpace(rd.Certification)
			if cert != "" {
				if strings.HasPrefix(strings.ToUpper(cert), "RATED ") || strings.EqualFold(cert, "NR") || strings.EqualFold(cert, "UNRATED") {
					return cert
				}
				return "Rated " + cert
			}
		}
	}
	return ""
}

// TMDBNamedItem represents a TMDB genre, studio, or network item.
type TMDBNamedItem struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// TMDBCredits represents cast and crew credits returned by TMDB.
type TMDBCredits struct {
	Cast []TMDBCastMember `json:"cast"`
	Crew []TMDBCrewMember `json:"crew"`
}

// TMDBCastMember represents an actor entry in TMDB credits.
type TMDBCastMember struct {
	Name      string `json:"name"`
	Character string `json:"character"`
	Order     int    `json:"order"`
}

// TMDBCrewMember represents a crew entry in TMDB credits.
type TMDBCrewMember struct {
	Name       string `json:"name"`
	Job        string `json:"job"`
	Department string `json:"department"`
}

// TMDBReleaseDatesEnvelope wraps country release date certifications for a movie.
type TMDBReleaseDatesEnvelope struct {
	Results []struct {
		ISO3166_1    string `json:"iso_3166_1"`
		ReleaseDates []struct {
			Certification string `json:"certification"`
			Type          int    `json:"type"`
		} `json:"release_dates"`
	} `json:"results"`
}

// TMDBTVSearchResult represents an entry returned by GET /3/search/tv.
type TMDBTVSearchResult struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	OriginalName string `json:"original_name"`
	FirstAirDate string `json:"first_air_date"`
	Overview     string `json:"overview"`
	PosterPath   string `json:"poster_path"`
}

// Year parses the 4-digit first air year from FirstAirDate ("YYYY-MM-DD").
func (s TMDBTVSearchResult) Year() int {
	return parseYearPrefix(s.FirstAirDate)
}

// TMDBTVDetails represents the response from GET /3/tv/{series_id}?append_to_response=credits,content_ratings,external_ids.
type TMDBTVDetails struct {
	ID                  int             `json:"id"`
	Name                string          `json:"name"`
	OriginalName        string          `json:"original_name"`
	FirstAirDate        string          `json:"first_air_date"`
	Overview            string          `json:"overview"`
	PosterPath          string          `json:"poster_path"`
	BackdropPath        string          `json:"backdrop_path"`
	Genres              []TMDBNamedItem `json:"genres"`
	Networks            []TMDBNamedItem `json:"networks"`
	ProductionCompanies []TMDBNamedItem `json:"production_companies"`
	ExternalIDs         struct {
		IMDbID string `json:"imdb_id"`
	} `json:"external_ids"`
	ContentRatings struct {
		Results []struct {
			ISO3166_1 string `json:"iso_3166_1"`
			Rating    string `json:"rating"`
		} `json:"results"`
	} `json:"content_ratings"`
}

// Year parses the 4-digit first air year from FirstAirDate ("YYYY-MM-DD").
func (t *TMDBTVDetails) Year() int {
	if t == nil {
		return 0
	}
	return parseYearPrefix(t.FirstAirDate)
}

// USContentRating extracts the US TV parental rating (e.g. "TV-14") from content_ratings.
func (t *TMDBTVDetails) USContentRating() string {
	if t == nil {
		return ""
	}
	for _, r := range t.ContentRatings.Results {
		if strings.EqualFold(r.ISO3166_1, "US") && strings.TrimSpace(r.Rating) != "" {
			return strings.TrimSpace(r.Rating)
		}
	}
	return ""
}

// TMDBSeasonDetails represents the response from GET /3/tv/{series_id}/season/{season_number}.
type TMDBSeasonDetails struct {
	ID           int           `json:"id"`
	SeasonNumber int           `json:"season_number"`
	Name         string        `json:"name"`
	Episodes     []TMDBEpisode `json:"episodes"`
}

// TMDBEpisode represents a single episode inside TMDBSeasonDetails.
type TMDBEpisode struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	SeasonNumber  int    `json:"season_number"`
	EpisodeNumber int    `json:"episode_number"`
	AirDate       string `json:"air_date"`
	Runtime       int    `json:"runtime"`
	Overview      string `json:"overview"`
	StillPath     string `json:"still_path"`
}

// TMDBClient queries the TMDB REST API using either a v3 API key (?api_key=)
// or a v4 Read Access Token (Authorization: Bearer).
type TMDBClient struct {
	APIKey        string
	BaseURL       string
	HTTPClient    *http.Client
	RetryBackoffs []time.Duration
	Sleep         func(ctx context.Context, d time.Duration) error
}

// NewTMDBClient returns a TMDBClient configured with production defaults.
func NewTMDBClient(apiKey string) *TMDBClient {
	return &TMDBClient{
		APIKey:        strings.TrimSpace(apiKey),
		BaseURL:       DefaultTMDBBaseURL,
		HTTPClient:    &http.Client{Timeout: 10 * time.Second},
		RetryBackoffs: DefaultRetryBackoffs,
		Sleep:         defaultContextSleep,
	}
}

// IsBearerToken returns true if apiKey is a TMDB v4 JWT Read Access Token
// (starts with "eyJ" or length > 64) per METADATA.md Section 3.4.1.
func IsBearerToken(apiKey string) bool {
	trimmed := strings.TrimSpace(apiKey)
	return strings.HasPrefix(trimmed, "eyJ") || len(trimmed) > 64
}

// SearchMovies queries GET /3/search/movie.
func (c *TMDBClient) SearchMovies(ctx context.Context, query string) ([]TMDBMovieSearchResult, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("language", "en-US")

	var resp struct {
		Results []TMDBMovieSearchResult `json:"results"`
	}
	if err := c.getJSON(ctx, "/search/movie", params, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// GetMovieDetails queries GET /3/movie/{movie_id}?append_to_response=credits,release_dates&language=en-US.
func (c *TMDBClient) GetMovieDetails(ctx context.Context, movieID int) (*TMDBMovieDetails, error) {
	params := url.Values{}
	params.Set("append_to_response", "credits,release_dates")
	params.Set("language", "en-US")

	var details TMDBMovieDetails
	if err := c.getJSON(ctx, fmt.Sprintf("/movie/%d", movieID), params, &details); err != nil {
		return nil, err
	}
	return &details, nil
}

// SearchTV queries GET /3/search/tv.
func (c *TMDBClient) SearchTV(ctx context.Context, query string) ([]TMDBTVSearchResult, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("language", "en-US")

	var resp struct {
		Results []TMDBTVSearchResult `json:"results"`
	}
	if err := c.getJSON(ctx, "/search/tv", params, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// GetTVDetails queries GET /3/tv/{series_id}?append_to_response=credits,content_ratings,external_ids&language=en-US.
func (c *TMDBClient) GetTVDetails(ctx context.Context, seriesID int) (*TMDBTVDetails, error) {
	params := url.Values{}
	params.Set("append_to_response", "credits,content_ratings,external_ids")
	params.Set("language", "en-US")

	var details TMDBTVDetails
	if err := c.getJSON(ctx, fmt.Sprintf("/tv/%d", seriesID), params, &details); err != nil {
		return nil, err
	}
	return &details, nil
}

// GetTVSeason queries GET /3/tv/{series_id}/season/{season_number}?language=en-US.
func (c *TMDBClient) GetTVSeason(ctx context.Context, seriesID, seasonNumber int) (*TMDBSeasonDetails, error) {
	params := url.Values{}
	params.Set("language", "en-US")

	var season TMDBSeasonDetails
	if err := c.getJSON(ctx, fmt.Sprintf("/tv/%d/season/%d", seriesID, seasonNumber), params, &season); err != nil {
		return nil, err
	}
	return &season, nil
}

func (c *TMDBClient) getJSON(ctx context.Context, endpoint string, params url.Values, dest any) error {
	apiKey := strings.TrimSpace(c.APIKey)
	if apiKey == "" {
		return errors.New("tmdb api key is not configured")
	}

	baseURL := strings.TrimRight(c.BaseURL, "/")
	if baseURL == "" {
		baseURL = DefaultTMDBBaseURL
	}

	reqParams := url.Values{}
	for k, v := range params {
		reqParams[k] = v
	}
	useBearer := IsBearerToken(apiKey)
	if !useBearer {
		reqParams.Set("api_key", apiKey)
	}

	fullURL := baseURL + endpoint
	if encoded := reqParams.Encode(); encoded != "" {
		fullURL += "?" + encoded
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	sleepFn := c.Sleep
	if sleepFn == nil {
		sleepFn = defaultContextSleep
	}
	backoffs := c.RetryBackoffs
	if backoffs == nil {
		backoffs = DefaultRetryBackoffs
	}

	var lastErr error
	maxAttempts := 1 + len(backoffs)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepFn(ctx, backoffs[attempt-1]); err != nil {
				return err
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if useBearer {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		// Retry on HTTP 429 rate limit or 5xx server error
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("tmdb HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("tmdb HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		if err := json.Unmarshal(body, dest); err != nil {
			return fmt.Errorf("unmarshal tmdb response: %w", err)
		}
		return nil
	}

	return fmt.Errorf("tmdb request failed after %d attempts: %w", maxAttempts, lastErr)
}

func defaultContextSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RoutingDecision encapsulates the classification, TMDB match result, and routing target
// produced by EvaluateDisc per METADATA.md Section 3.3 & 3.4.
type RoutingDecision struct {
	RawLabel       string
	SanitizedQuery string
	Classification string
	AutoFileMovie  bool
	Reason         string
	Hints          LabelHints
	TracksSeconds  []float64
	MatchedMovie   *TMDBMovieDetails
	Candidates     []TMDBCandidate
}

// EvaluateDisc runs volume label sanitization, track-length classification, and TMDB
// matching/routing rules per METADATA.md Sections 2 & 3.
func EvaluateDisc(ctx context.Context, rawLabel string, tracksSeconds []float64, tmdb *TMDBClient) RoutingDecision {
	sanitized := SanitizeLabel(rawLabel)
	classRes := ClassifyTracks(tracksSeconds, rawLabel)

	hints := sanitized.Hints
	hints.EstimatedEpisodes = classRes.EstimatedEpisodes
	classification := classRes.MediaType

	// Per METADATA.md Section 3.3 flowchart:
	// "Classification == TV_SERIES or HintSeason Present?" -> TV Series Disc
	isTVSeries := classification == MediaTypeTVSeries || hints.Season > 0
	if isTVSeries {
		classification = MediaTypeTVSeries
		if hints.Season <= 0 {
			hints.Season = 1 // Default HintSeason to 1 if absent (METADATA.md Section 3.3 Policy 1)
		}
	}

	decision := RoutingDecision{
		RawLabel:       strings.TrimSpace(rawLabel),
		SanitizedQuery: sanitized.Query,
		Classification: classification,
		Hints:          hints,
		TracksSeconds:   nonNilTracks(tracksSeconds),
		Candidates:     []TMDBCandidate{},
	}

	// Policy 1: TV Series Discs Always Route to /unidentified for 1-Click Confirmation
	if isTVSeries {
		decision.Reason = ReasonTVSeriesConfirmation
		if !sanitized.IsGeneric && tmdb != nil {
			results, err := tmdb.SearchTV(ctx, sanitized.Query)
			if err != nil {
				decision.Reason = ReasonTMDBLookupError
				return decision
			}
			for _, item := range results {
				decision.Candidates = append(decision.Candidates, TMDBCandidate{
					ID:         item.ID,
					MediaType:  "tv",
					Title:      item.Name,
					Year:       item.Year(),
					Overview:   item.Overview,
					PosterPath: item.PosterPath,
				})
			}
		}
		return decision
	}

	// Generic label check (METADATA.md Section 3.1)
	if sanitized.IsGeneric {
		decision.Reason = ReasonGenericLabel
		return decision
	}

	// Unidentified media type check
	if classification != MediaTypeMovie {
		decision.Reason = ReasonUnidentifiedMedia
		return decision
	}

	// Policy 2: High-Confidence Movie Auto-Filing Criteria (METADATA.md Section 3.3)
	if tmdb == nil {
		decision.Reason = ReasonTMDBLookupError
		return decision
	}

	searchResults, err := tmdb.SearchMovies(ctx, sanitized.Query)
	if err != nil {
		decision.Reason = ReasonTMDBLookupError
		return decision
	}

	topN := len(searchResults)
	if topN > MaxMovieCandidatesToInspect {
		topN = MaxMovieCandidatesToInspect
	}

	var confidentMatches []*TMDBMovieDetails
	for i := 0; i < topN; i++ {
		sr := searchResults[i]
		decision.Candidates = append(decision.Candidates, TMDBCandidate{
			ID:         sr.ID,
			MediaType:  "movie",
			Title:      sr.Title,
			Year:       sr.Year(),
			Overview:   sr.Overview,
			PosterPath: sr.PosterPath,
		})

		// Criterion 2: Exact normalized title match on title or original_title
		if !TitlesMatchNormalized(sanitized.Query, sr.Title, sr.OriginalTitle) {
			continue
		}

		// Criterion 3: Fetch /3/movie/{id} and verify runtime within ±15m of a feature track
		details, err := tmdb.GetMovieDetails(ctx, sr.ID)
		if err != nil {
			decision.Reason = ReasonTMDBLookupError
			return decision
		}
		if !TitlesMatchNormalized(sanitized.Query, details.Title, details.OriginalTitle) &&
			!TitlesMatchNormalized(sanitized.Query, sr.Title, sr.OriginalTitle) {
			continue
		}
		if matchesAnyFeatureRuntime(details.Runtime, classRes.FeatureTracks) {
			confidentMatches = append(confidentMatches, details)
		}
	}

	// Criterion 4: Unambiguous Match (No Competing Remake), or HintYear disambiguates within ±1 year
	if hints.Year > 0 && len(confidentMatches) > 0 {
		var yearFiltered []*TMDBMovieDetails
		for _, m := range confidentMatches {
			if y := m.Year(); y > 0 && intAbs(y-hints.Year) <= 1 {
				yearFiltered = append(yearFiltered, m)
			}
		}
		confidentMatches = yearFiltered
	}

	switch len(confidentMatches) {
	case 1:
		decision.AutoFileMovie = true
		decision.MatchedMovie = confidentMatches[0]
		decision.Reason = ""
		// Ensure the matched movie is first in Candidates in case a target collision routes it to /unidentified
		promoteMatchedMovieCandidate(&decision, confidentMatches[0])
	case 0:
		decision.Reason = ReasonNoConfidentMatch
	default:
		decision.Reason = ReasonAmbiguousMatch
	}

	return decision
}

func promoteMatchedMovieCandidate(decision *RoutingDecision, matched *TMDBMovieDetails) {
	if matched == nil {
		return
	}
	cand := TMDBCandidate{
		ID:         matched.ID,
		MediaType:  "movie",
		Title:      matched.Title,
		Year:       matched.Year(),
		Overview:   matched.Overview,
		PosterPath: matched.PosterPath,
	}
	reordered := []TMDBCandidate{cand}
	for _, existing := range decision.Candidates {
		if existing.ID != matched.ID {
			reordered = append(reordered, existing)
		}
	}
	decision.Candidates = reordered
}

func matchesAnyFeatureRuntime(tmdbRuntimeMinutes int, featureTracksSeconds []float64) bool {
	if tmdbRuntimeMinutes <= 0 {
		return false
	}
	for _, trackSec := range featureTracksSeconds {
		trackMinutes := trackSec / 60.0
		if math.Abs(float64(tmdbRuntimeMinutes)-trackMinutes) <= RuntimeToleranceMinutes {
			return true
		}
	}
	return false
}

// TitlesMatchNormalized returns true if query matches candidateTitle or candidateOriginalTitle
// after lowercasing, stripping punctuation, and optionally stripping leading articles ("the", "a", "an").
func TitlesMatchNormalized(query, candidateTitle, candidateOriginalTitle string) bool {
	return singleTitleMatchesNormalized(query, candidateTitle) ||
		singleTitleMatchesNormalized(query, candidateOriginalTitle)
}

func singleTitleMatchesNormalized(a, b string) bool {
	aFull, aNoArt := normalizeTitleForms(a)
	bFull, bNoArt := normalizeTitleForms(b)
	if aFull == "" || bFull == "" {
		return false
	}
	if aFull == bFull {
		return true
	}
	return aNoArt != "" && aNoArt == bNoArt
}

func normalizeTitleForms(s string) (full string, withoutArticle string) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "&", " and ")

	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastSpace = false
		} else if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}

	full = strings.TrimSpace(b.String())
	withoutArticle = full
	for _, art := range []string{"the ", "an ", "a "} {
		if strings.HasPrefix(full, art) {
			rest := strings.TrimSpace(strings.TrimPrefix(full, art))
			if rest != "" {
				withoutArticle = rest
			}
			break
		}
	}
	return full, withoutArticle
}

func parseYearPrefix(dateStr string) int {
	dateStr = strings.TrimSpace(dateStr)
	if len(dateStr) < 4 {
		return 0
	}
	y, err := strconv.Atoi(dateStr[:4])
	if err != nil {
		return 0
	}
	return y
}

func intAbs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func nonNilTracks(tracks []float64) []float64 {
	if tracks == nil {
		return []float64{}
	}
	out := make([]float64, len(tracks))
	copy(out, tracks)
	return out
}
