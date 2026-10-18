package metadata

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

const xmlHeaderStandalone = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"

// ProbeSidecar represents the <RAW_LABEL>_<TIMESTAMP>.probe.json file persisted in
// <NFS_ROOT>/unidentified/ per METADATA.md Section 4.1.
type ProbeSidecar struct {
	ISOFilename    string          `json:"iso_filename"`
	ProbedAt       string          `json:"probed_at"`
	RawLabel       string          `json:"raw_label"`
	SanitizedQuery string          `json:"sanitized_query"`
	Classification string          `json:"classification"`
	Reason         string          `json:"reason"`
	Hints          LabelHints      `json:"hints"`
	TracksSeconds  []float64       `json:"tracks_seconds"`
	TMDBCandidates []TMDBCandidate `json:"tmdb_candidates"`
}

// MarshalProbeSidecar serializes a ProbeSidecar to indented JSON with non-nil slices.
func MarshalProbeSidecar(p *ProbeSidecar) ([]byte, error) {
	if p == nil {
		return nil, errors.New("nil ProbeSidecar")
	}
	cp := *p
	if cp.TracksSeconds == nil {
		cp.TracksSeconds = []float64{}
	}
	if cp.TMDBCandidates == nil {
		cp.TMDBCandidates = []TMDBCandidate{}
	}
	data, err := json.MarshalIndent(&cp, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// ReadProbeSidecar reads and unmarshals a .probe.json file from disk.
func ReadProbeSidecar(path string) (*ProbeSidecar, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p ProbeSidecar
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if p.TracksSeconds == nil {
		p.TracksSeconds = []float64{}
	}
	if p.TMDBCandidates == nil {
		p.TMDBCandidates = []TMDBCandidate{}
	}
	return &p, nil
}

// SanitizeFilesystemName sanitizes a TMDB title for creating directories or filenames
// on the NFS share per METADATA.md Section 5.1:
//  1. Replace ": " with " - " (e.g. "Star Wars: Episode IV" -> "Star Wars - Episode IV").
//  2. Strip or replace reserved filesystem characters (/, \, :, *, ?, ", <, >, |) with spaces.
//  3. Collapse consecutive spaces and trim leading/trailing dots and spaces.
func SanitizeFilesystemName(name string) string {
	s := strings.ReplaceAll(name, ": ", " - ")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteByte(' ')
		default:
			if r < 32 {
				b.WriteByte(' ')
			} else {
				b.WriteRune(r)
			}
		}
	}
	collapsed := multiSpaceRe.ReplaceAllString(b.String(), " ")
	return strings.Trim(collapsed, " .")
}

// FormatMovieBaseName returns "Movie Title (Year)" (or "Movie Title" if year <= 0)
// sanitized for filesystem paths, optionally appending " - <EditionSuffix>".
func FormatMovieBaseName(title string, year int, editionSuffix string) string {
	safeTitle := SanitizeFilesystemName(title)
	if safeTitle == "" {
		safeTitle = "Untitled Movie"
	}
	base := safeTitle
	if year > 0 {
		base = fmt.Sprintf("%s (%d)", safeTitle, year)
	}
	suffix := SanitizeFilesystemName(editionSuffix)
	if suffix != "" {
		base = fmt.Sprintf("%s - %s", base, suffix)
	}
	return base
}

// FormatShowDirName returns "Show Title (Year)" (or "Show Title" if year <= 0) sanitized for NFS paths.
func FormatShowDirName(title string, year int) string {
	safeTitle := SanitizeFilesystemName(title)
	if safeTitle == "" {
		safeTitle = "Untitled Show"
	}
	if year > 0 {
		return fmt.Sprintf("%s (%d)", safeTitle, year)
	}
	return safeTitle
}

// FormatSeasonDirName returns "Season XX" (zero-padded to at least 2 digits).
func FormatSeasonDirName(season int) string {
	if season <= 0 {
		season = 1
	}
	return fmt.Sprintf("Season %02d", season)
}

// FormatMultiEpisodeBaseName returns "Show Title - S01E01E02..." per METADATA.md Section 5.3.
func FormatMultiEpisodeBaseName(showTitle string, season int, episodeNumbers []int) string {
	safeShow := SanitizeFilesystemName(showTitle)
	if safeShow == "" {
		safeShow = "Untitled Show"
	}
	if season <= 0 {
		season = 1
	}
	eps := make([]int, len(episodeNumbers))
	copy(eps, episodeNumbers)
	sort.Ints(eps)

	var epPart strings.Builder
	fmt.Fprintf(&epPart, "S%02d", season)
	for _, ep := range eps {
		if ep > 0 {
			fmt.Fprintf(&epPart, "E%02d", ep)
		}
	}
	return fmt.Sprintf("%s - %s", safeShow, epPart.String())
}

type nfoUniqueID struct {
	Type    string `xml:"type,attr"`
	Default string `xml:"default,attr,omitempty"`
	Value   string `xml:",chardata"`
}

type nfoActor struct {
	Name  string `xml:"name"`
	Role  string `xml:"role,omitempty"`
	Order int    `xml:"order"`
}

type nfoThumb struct {
	Aspect string `xml:"aspect,attr,omitempty"`
	URL    string `xml:",chardata"`
}

type nfoFanart struct {
	Thumbs []string `xml:"thumb"`
}

type kodiMovieNFO struct {
	XMLName       xml.Name      `xml:"movie"`
	Title         string        `xml:"title"`
	OriginalTitle string        `xml:"originaltitle,omitempty"`
	Year          int           `xml:"year,omitempty"`
	Premiered     string        `xml:"premiered,omitempty"`
	Runtime       int           `xml:"runtime,omitempty"`
	Tagline       string        `xml:"tagline,omitempty"`
	Plot          string        `xml:"plot,omitempty"`
	MPAA          string        `xml:"mpaa,omitempty"`
	Rating        string        `xml:"rating,omitempty"`
	Votes         int           `xml:"votes,omitempty"`
	UniqueIDs     []nfoUniqueID `xml:"uniqueid"`
	Genres        []string      `xml:"genre,omitempty"`
	Studios       []string      `xml:"studio,omitempty"`
	Directors     []string      `xml:"director,omitempty"`
	Actors        []nfoActor    `xml:"actor,omitempty"`
	Thumbs        []nfoThumb    `xml:"thumb,omitempty"`
	Fanart        *nfoFanart    `xml:"fanart,omitempty"`
}

// GenerateMovieNFO builds a Kodi-compliant <movie> .nfo XML document from TMDB movie details
// per METADATA.md Section 5.2 & 5.4.
func GenerateMovieNFO(movie *TMDBMovieDetails, editionSuffix string) ([]byte, error) {
	if movie == nil {
		return nil, errors.New("nil TMDBMovieDetails")
	}

	title := strings.TrimSpace(movie.Title)
	if suffix := strings.TrimSpace(editionSuffix); suffix != "" {
		title = fmt.Sprintf("%s (%s)", title, suffix)
	}
	origTitle := strings.TrimSpace(movie.OriginalTitle)
	if origTitle == "" {
		origTitle = strings.TrimSpace(movie.Title)
	}

	nfo := kodiMovieNFO{
		Title:         title,
		OriginalTitle: origTitle,
		Year:          movie.Year(),
		Premiered:     strings.TrimSpace(movie.ReleaseDate),
		Runtime:       movie.Runtime,
		Tagline:       strings.TrimSpace(movie.Tagline),
		Plot:          strings.TrimSpace(movie.Overview),
		MPAA:          movie.USCertification(),
		Votes:         movie.VoteCount,
	}

	if movie.VoteAverage > 0 {
		rounded := math.Round(movie.VoteAverage*10) / 10
		nfo.Rating = strconv.FormatFloat(rounded, 'f', -1, 64)
	}

	if movie.ID > 0 {
		nfo.UniqueIDs = append(nfo.UniqueIDs, nfoUniqueID{
			Type:    "tmdb",
			Default: "true",
			Value:   strconv.Itoa(movie.ID),
		})
	}
	if imdb := strings.TrimSpace(movie.IMDbID); imdb != "" {
		nfo.UniqueIDs = append(nfo.UniqueIDs, nfoUniqueID{
			Type:  "imdb",
			Value: imdb,
		})
	}

	for _, g := range movie.Genres {
		if name := strings.TrimSpace(g.Name); name != "" {
			nfo.Genres = append(nfo.Genres, name)
		}
	}
	for _, s := range movie.ProductionCompanies {
		if name := strings.TrimSpace(s.Name); name != "" {
			nfo.Studios = append(nfo.Studios, name)
		}
	}
	for _, crew := range movie.Credits.Crew {
		if strings.EqualFold(strings.TrimSpace(crew.Job), "Director") {
			if name := strings.TrimSpace(crew.Name); name != "" {
				nfo.Directors = append(nfo.Directors, name)
			}
		}
	}
	for _, cast := range movie.Credits.Cast {
		name := strings.TrimSpace(cast.Name)
		if name == "" {
			continue
		}
		nfo.Actors = append(nfo.Actors, nfoActor{
			Name:  name,
			Role:  strings.TrimSpace(cast.Character),
			Order: cast.Order,
		})
	}

	if poster := fullTMDBImageURL(movie.PosterPath); poster != "" {
		nfo.Thumbs = append(nfo.Thumbs, nfoThumb{
			Aspect: "poster",
			URL:    poster,
		})
	}
	if backdrop := fullTMDBImageURL(movie.BackdropPath); backdrop != "" {
		nfo.Fanart = &nfoFanart{
			Thumbs: []string{backdrop},
		}
	}

	body, err := xml.MarshalIndent(nfo, "", "  ")
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString(xmlHeaderStandalone)
	out.Write(body)
	out.WriteByte('\n')
	return out.Bytes(), nil
}

type kodiTVShowNFO struct {
	XMLName       xml.Name      `xml:"tvshow"`
	Title         string        `xml:"title"`
	OriginalTitle string        `xml:"originaltitle,omitempty"`
	Year          int           `xml:"year,omitempty"`
	Premiered     string        `xml:"premiered,omitempty"`
	Plot          string        `xml:"plot,omitempty"`
	MPAA          string        `xml:"mpaa,omitempty"`
	UniqueIDs     []nfoUniqueID `xml:"uniqueid"`
	Genres        []string      `xml:"genre,omitempty"`
	Studios       []string      `xml:"studio,omitempty"`
	Thumbs        []nfoThumb    `xml:"thumb,omitempty"`
	Fanart        *nfoFanart    `xml:"fanart,omitempty"`
}

// GenerateTVShowNFO builds a Kodi-compliant <tvshow> tvshow.nfo XML document from TMDB TV details
// per METADATA.md Section 5.3.
func GenerateTVShowNFO(show *TMDBTVDetails) ([]byte, error) {
	if show == nil {
		return nil, errors.New("nil TMDBTVDetails")
	}

	origTitle := strings.TrimSpace(show.OriginalName)
	if origTitle == "" {
		origTitle = strings.TrimSpace(show.Name)
	}

	nfo := kodiTVShowNFO{
		Title:         strings.TrimSpace(show.Name),
		OriginalTitle: origTitle,
		Year:          show.Year(),
		Premiered:     strings.TrimSpace(show.FirstAirDate),
		Plot:          strings.TrimSpace(show.Overview),
		MPAA:          show.USContentRating(),
	}

	if show.ID > 0 {
		nfo.UniqueIDs = append(nfo.UniqueIDs, nfoUniqueID{
			Type:    "tmdb",
			Default: "true",
			Value:   strconv.Itoa(show.ID),
		})
	}
	if imdb := strings.TrimSpace(show.ExternalIDs.IMDbID); imdb != "" {
		nfo.UniqueIDs = append(nfo.UniqueIDs, nfoUniqueID{
			Type:  "imdb",
			Value: imdb,
		})
	}

	for _, g := range show.Genres {
		if name := strings.TrimSpace(g.Name); name != "" {
			nfo.Genres = append(nfo.Genres, name)
		}
	}
	studios := show.Networks
	if len(studios) == 0 {
		studios = show.ProductionCompanies
	}
	for _, s := range studios {
		if name := strings.TrimSpace(s.Name); name != "" {
			nfo.Studios = append(nfo.Studios, name)
		}
	}

	if poster := fullTMDBImageURL(show.PosterPath); poster != "" {
		nfo.Thumbs = append(nfo.Thumbs, nfoThumb{
			Aspect: "poster",
			URL:    poster,
		})
	}
	if backdrop := fullTMDBImageURL(show.BackdropPath); backdrop != "" {
		nfo.Fanart = &nfoFanart{
			Thumbs: []string{backdrop},
		}
	}

	body, err := xml.MarshalIndent(nfo, "", "  ")
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString(xmlHeaderStandalone)
	out.Write(body)
	out.WriteByte('\n')
	return out.Bytes(), nil
}

type kodiEpisodeDetailsNFO struct {
	XMLName   xml.Name      `xml:"episodedetails"`
	Title     string        `xml:"title"`
	ShowTitle string        `xml:"showtitle,omitempty"`
	Season    int           `xml:"season"`
	Episode   int           `xml:"episode"`
	Aired     string        `xml:"aired,omitempty"`
	Runtime   int           `xml:"runtime,omitempty"`
	Plot      string        `xml:"plot,omitempty"`
	UniqueIDs []nfoUniqueID `xml:"uniqueid,omitempty"`
	Thumb     string        `xml:"thumb,omitempty"`
}

// GenerateMultiEpisodeNFO builds a Kodi-compliant multi-episode .nfo XML document containing
// sequential root-level <episodedetails> elements without an outer wrapper tag per METADATA.md Section 5.3.
func GenerateMultiEpisodeNFO(showTitle string, episodes []TMDBEpisode) ([]byte, error) {
	if len(episodes) == 0 {
		return nil, errors.New("at least one episode is required for multi-episode NFO")
	}

	var out bytes.Buffer
	out.WriteString(xmlHeaderStandalone)

	for _, ep := range episodes {
		epNode := kodiEpisodeDetailsNFO{
			Title:     strings.TrimSpace(ep.Name),
			ShowTitle: strings.TrimSpace(showTitle),
			Season:    ep.SeasonNumber,
			Episode:   ep.EpisodeNumber,
			Aired:     strings.TrimSpace(ep.AirDate),
			Runtime:   ep.Runtime,
			Plot:      strings.TrimSpace(ep.Overview),
			Thumb:     fullTMDBImageURL(ep.StillPath),
		}
		if ep.ID > 0 {
			epNode.UniqueIDs = []nfoUniqueID{
				{
					Type:    "tmdb",
					Default: "true",
					Value:   strconv.Itoa(ep.ID),
				},
			}
		}
		block, err := xml.MarshalIndent(epNode, "", "  ")
		if err != nil {
			return nil, err
		}
		out.Write(block)
		out.WriteByte('\n')
	}

	return out.Bytes(), nil
}

func fullTMDBImageURL(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return TMDBImageBaseURL + path
}
