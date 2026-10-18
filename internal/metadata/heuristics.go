package metadata

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const (
	MediaTypeMovie        = "MOVIE"
	MediaTypeTVSeries     = "TV_SERIES"
	MediaTypeUnidentified = "UNIDENTIFIED"

	MinTrackDurationSeconds        = 18 * 60.0  // 1,080s (18 minutes)
	MaxEpisodeDurationSeconds      = 65 * 60.0  // 3,900s (65 minutes)
	MaxBonusFeatureDurationSeconds = 45 * 60.0  // 2,700s (45 minutes)
	MinFeatureDurationSeconds      = 60 * 60.0  // 3,600s (60 minutes)
	MinPrecedenceFeatureSeconds    = 75 * 60.0  // 4,500s (75 minutes)
	MaxFeatureDurationSeconds      = 240 * 60.0 // 14,400s (240 minutes)
	DuplicatePGCDeltaSeconds       = 0.1
	PlayAllToleranceSeconds        = 300.0 // 5 minutes
	EpisodeClusterMedianDeltaSecs  = 900.0 // ±15 minutes
)

// ClassificationResult holds the result of the track-length classification heuristic.
type ClassificationResult struct {
	MediaType         string
	DedupedTracks     []float64
	FeatureTracks     []float64
	EpisodeTracks     []float64
	PlayAllTracks     []float64
	EstimatedEpisodes int
}

// LabelHints holds structured hints extracted from a DVD volume label.
type LabelHints struct {
	Season            int `json:"season"`
	Disc              int `json:"disc"`
	Year              int `json:"year"`
	EstimatedEpisodes int `json:"estimated_episodes"`
}

// SanitizedLabel holds the sanitized TMDB query and extracted hints for a DVD volume label.
type SanitizedLabel struct {
	RawLabel  string
	Query     string
	IsGeneric bool
	Hints     LabelHints
}

var genericLabelBlocklist = map[string]struct{}{
	"DVD_VIDEO":         {},
	"DVD_ROM":           {},
	"DVD":               {},
	"DISC":              {},
	"DISC1":             {},
	"DISC_1":            {},
	"VIDEO_TS":          {},
	"LOGICAL_VOLUME_ID": {},
	"UNDEFINED":         {},
	"UNTITLED":          {},
	"UNKNOWN":           {},
	"NEW":               {},
	"VOLUME_IDENTIFIER": {},
	"DVDVOLUME":         {},
	"CDROM":             {},
	"SONIC":             {},
}

var (
	tvLabelTokenRe       = regexp.MustCompile(`(?i)(?:\bS\d{1,2}\b|\bS\d{1,2}D\d{1,2}\b|\bSEASON\b|\bEPISODE\b)`)
	combinedSeasonDiscRe = regexp.MustCompile(`(?i)\bS(\d{1,2})D(\d{1,2})\b`)
	seasonHintRe         = regexp.MustCompile(`(?i)\b(?:S|SEASON\s*)(\d{1,2})\b`)
	discHintRe           = regexp.MustCompile(`(?i)\b(?:D|DISC\s*|DISK\s*)(\d{1,2})\b`)
	trailingYearRe       = regexp.MustCompile(`^(.+?)\s+(19\d{2}|20\d{2})$`)
	multiSpaceRe         = regexp.MustCompile(`\s+`)
)

var multiWordTechPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bSPECIAL\s+EDITION\b`),
	regexp.MustCompile(`(?i)\bCOLLECTOR'?S?\s+EDITION\b`),
	regexp.MustCompile(`(?i)\bDIRECTOR'?S?\s+CUT\b`),
	regexp.MustCompile(`(?i)\bANNIVERSARY\s+EDITION\b`),
	regexp.MustCompile(`(?i)\b16[_\s]9\b`),
	regexp.MustCompile(`(?i)\b4[_\s]3\b`),
	regexp.MustCompile(`(?i)\bP&S\b`),
}

var singleWordTechTokens = map[string]struct{}{
	// Aspect Ratio / Format
	"16X9": {}, "4X3": {}, "16_9": {}, "4_3": {}, "WS": {}, "FS": {}, "LBX": {},
	"WIDESCREEN": {}, "FULLSCREEN": {}, "LETTERBOX": {}, "ANAMORPHIC": {}, "PANANDSCAN": {}, "P&S": {},
	// Video / Audio / Region Standards
	"NTSC": {}, "PAL": {}, "DVD": {}, "DVD5": {}, "DVD9": {}, "VIDEO": {},
	"AC3": {}, "DTS": {}, "DOLBY": {}, "REG1": {}, "REG2": {}, "R1": {}, "R2": {},
	"US": {}, "NA": {}, "UK": {}, "RENTAL": {}, "RETAIL": {},
	// Edition Markers
	"SE": {}, "CE": {}, "LE": {}, "EXTENDED": {}, "UNRATED": {}, "THEATRICAL": {}, "BONUS": {},
}

// ClassifyTracks executes the 4-step track-length heuristic defined in METADATA.md Section 2.
func ClassifyTracks(tracksSeconds []float64, rawLabel string) ClassificationResult {
	res := ClassificationResult{
		MediaType:     MediaTypeUnidentified,
		DedupedTracks: []float64{},
		FeatureTracks: []float64{},
		EpisodeTracks: []float64{},
		PlayAllTracks: []float64{},
	}

	// Step 1 & 2: Filter tracks < 18m (< 1,080s) and deduplicate identical-duration PGCs (Δ < 0.1s)
	for _, dur := range tracksSeconds {
		if dur < MinTrackDurationSeconds {
			continue
		}
		isDup := false
		for _, kept := range res.DedupedTracks {
			if math.Abs(dur-kept) < DuplicatePGCDeltaSeconds {
				isDup = true
				break
			}
		}
		if !isDup {
			res.DedupedTracks = append(res.DedupedTracks, dur)
		}
	}

	if len(res.DedupedTracks) == 0 {
		return res
	}

	// Step 3: Detect and exclude composite "Play All" tracks on TV discs.
	// Candidate episode tracks are in [1,080s, 3,900s] (18–65 minutes).
	isPlayAll := make([]bool, len(res.DedupedTracks))
	for i, longTrack := range res.DedupedTracks {
		if longTrack < MinFeatureDurationSeconds {
			continue
		}
		var sumEpisodes float64
		var countEpisodes int
		for j, cand := range res.DedupedTracks {
			if i == j {
				continue
			}
			if cand >= MinTrackDurationSeconds && cand <= MaxEpisodeDurationSeconds {
				sumEpisodes += cand
				countEpisodes++
			}
		}
		if countEpisodes >= 2 && math.Abs(longTrack-sumEpisodes) <= PlayAllToleranceSeconds {
			isPlayAll[i] = true
			res.PlayAllTracks = append(res.PlayAllTracks, longTrack)
		}
	}

	var episodeCandidates []float64
	var hasLongFeatureOver75m bool
	var allEpisodeCandidatesUnder45m = true

	for i, dur := range res.DedupedTracks {
		if isPlayAll[i] {
			continue
		}
		if dur >= MinTrackDurationSeconds && dur <= MaxEpisodeDurationSeconds {
			episodeCandidates = append(episodeCandidates, dur)
			if dur > MaxBonusFeatureDurationSeconds {
				allEpisodeCandidatesUnder45m = false
			}
		}
		if dur >= MinFeatureDurationSeconds && dur <= MaxFeatureDurationSeconds {
			res.FeatureTracks = append(res.FeatureTracks, dur)
			if dur > MinPrecedenceFeatureSeconds {
				hasLongFeatureOver75m = true
			}
		}
	}

	// Step 4: Media Type Classification Rules
	if len(episodeCandidates) >= 2 {
		med := medianDuration(episodeCandidates)
		var clustered []float64
		for _, dur := range episodeCandidates {
			if math.Abs(dur-med) <= EpisodeClusterMedianDeltaSecs {
				clustered = append(clustered, dur)
			}
		}

		if len(clustered) >= 2 {
			// Check Movie precedence rule (METADATA.md Section 2.4.1):
			// If the disc also contains a valid non-Play-All feature track > 75m (4,500s)
			// and there are only 2 tracks in the 18–45m range (typical bonus featurettes),
			// MOVIE takes precedence unless the volume label explicitly contains a TV token.
			moviePrecedence := hasLongFeatureOver75m &&
				len(episodeCandidates) == 2 &&
				allEpisodeCandidatesUnder45m &&
				!hasExplicitTVToken(rawLabel)

			if !moviePrecedence {
				res.MediaType = MediaTypeTVSeries
				res.EpisodeTracks = clustered
				res.EstimatedEpisodes = len(clustered)
				return res
			}
		}
	}

	if len(res.FeatureTracks) >= 1 {
		res.MediaType = MediaTypeMovie
		return res
	}

	return res
}

func medianDuration(vals []float64) float64 {
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2.0
}

func hasExplicitTVToken(rawLabel string) bool {
	normalized := strings.NewReplacer("_", " ", ".", " ", "-", " ").Replace(rawLabel)
	return tvLabelTokenRe.MatchString(normalized)
}

// IsGenericLabel returns true if rawLabel (trimmed and uppercased) matches the generic
// authoring placeholder blocklist or is empty.
func IsGenericLabel(rawLabel string) bool {
	upper := strings.ToUpper(strings.TrimSpace(rawLabel))
	if upper == "" {
		return true
	}
	_, blocked := genericLabelBlocklist[upper]
	return blocked
}

// SanitizeLabel extracts structured hints (Season, Disc, Year) and produces a clean
// Title Case TMDB search query per METADATA.md Section 3.1 & 3.2.
func SanitizeLabel(rawLabel string) SanitizedLabel {
	trimmedRaw := strings.TrimSpace(rawLabel)
	result := SanitizedLabel{
		RawLabel:  trimmedRaw,
		IsGeneric: IsGenericLabel(trimmedRaw),
	}

	if trimmedRaw == "" {
		return result
	}

	// Pre-strip multi-word / special technical tokens (such as 16_9, 4_3, P&S) before separator replacement
	work := trimmedRaw
	for _, re := range multiWordTechPatterns {
		work = re.ReplaceAllString(work, " ")
	}

	// Step 1: Normalize separators (_, ., -, and parentheses) to spaces; collapse multiple spaces
	work = strings.NewReplacer("_", " ", ".", " ", "-", " ", "(", " ", ")", " ").Replace(work)
	work = strings.TrimSpace(multiSpaceRe.ReplaceAllString(work, " "))

	// Pre-strip multi-word phrases again now that underscores/dots/hyphens are spaces
	for _, re := range multiWordTechPatterns {
		work = re.ReplaceAllString(work, " ")
	}
	work = strings.TrimSpace(multiSpaceRe.ReplaceAllString(work, " "))

	// Step 2: Extract TV Season / Disc Hints and strip matched tokens from the query
	if m := combinedSeasonDiscRe.FindStringSubmatch(work); len(m) == 3 {
		if s, err := strconv.Atoi(m[1]); err == nil {
			result.Hints.Season = s
		}
		if d, err := strconv.Atoi(m[2]); err == nil {
			result.Hints.Disc = d
		}
		work = combinedSeasonDiscRe.ReplaceAllString(work, " ")
	}
	if m := seasonHintRe.FindStringSubmatch(work); len(m) == 2 {
		if s, err := strconv.Atoi(m[1]); err == nil && result.Hints.Season == 0 {
			result.Hints.Season = s
		}
		work = seasonHintRe.ReplaceAllString(work, " ")
	}
	if m := discHintRe.FindStringSubmatch(work); len(m) == 2 {
		if d, err := strconv.Atoi(m[1]); err == nil && result.Hints.Disc == 0 {
			result.Hints.Disc = d
		}
		work = discHintRe.ReplaceAllString(work, " ")
	}
	work = strings.TrimSpace(multiSpaceRe.ReplaceAllString(work, " "))

	// Step 4 (first pass): Strip technical & edition tokens so any trailing release year is exposed
	work = stripSingleWordTechTokens(work)

	// Step 3: Extract trailing (or parenthesized) 4-digit release year (19xx or 20xx)
	// when it is not the entire title itself (e.g. movie "1917" or "2012").
	if m := trailingYearRe.FindStringSubmatch(work); len(m) == 3 {
		prefix := strings.TrimSpace(m[1])
		if countAlnum(prefix) >= 2 {
			if y, err := strconv.Atoi(m[2]); err == nil {
				result.Hints.Year = y
				work = prefix
			}
		}
	}

	// Step 4 (second pass): Strip any technical tokens that preceded the year
	work = stripSingleWordTechTokens(work)

	// Step 5: Final search query in Title Case
	result.Query = toTitleCase(work)
	if countAlnum(result.Query) < 2 {
		result.IsGeneric = true
	}

	return result
}

func stripSingleWordTechTokens(s string) string {
	fields := strings.Fields(s)
	var kept []string
	for _, f := range fields {
		upper := strings.ToUpper(f)
		if _, isTech := singleWordTechTokens[upper]; isTech {
			continue
		}
		kept = append(kept, f)
	}
	return strings.Join(kept, " ")
}

func countAlnum(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n
}

func toTitleCase(s string) string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(s)))
	for i, w := range fields {
		runes := []rune(w)
		for j, r := range runes {
			if unicode.IsLetter(r) {
				runes[j] = unicode.ToUpper(r)
				break
			}
		}
		fields[i] = string(runes)
	}
	return strings.Join(fields, " ")
}
