package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brotherlogic/hometheatre/internal/drive"
	"github.com/brotherlogic/hometheatre/internal/history"
	"github.com/brotherlogic/hometheatre/internal/metadata"
	"github.com/brotherlogic/hometheatre/internal/nfs"
	"github.com/brotherlogic/hometheatre/internal/ripper"
	"github.com/brotherlogic/hometheatre/internal/webhook"
)

// DriveStatusProvider returns the live optical drive status from drive.Poller.
type DriveStatusProvider interface {
	Status() (driveStatus int, latched bool, label string)
}

// RipperStatusProvider returns the live ripping pipeline status from ripper.Worker.
type RipperStatusProvider interface {
	Status() string
}

// Server serves the embedded Go Web UI (net/http + html/template) and JSON action endpoints.
type Server struct {
	Poller             DriveStatusProvider
	Ripper             RipperStatusProvider
	NFSWriter          *nfs.Writer
	TMDB               *metadata.TMDBClient
	History            *history.Store
	Logs               *history.LogBuffer
	Webhook            *webhook.Notifier
	QueueCountFunc     func() int
	Logger             *log.Logger

	tmpl *template.Template
	mux  *http.ServeMux
}

// UnidentifiedView wraps a ProbeSidecar with pre-computed UI display fields for the template.
type UnidentifiedView struct {
	Sidecar          *metadata.ProbeSidecar
	DefaultMediaType string // "movie" or "tv"
	DefaultTMDBID    int
	DefaultTitle     string
	DefaultYear      int
	DefaultSeason    int
	DefaultStartEp   int
	DefaultEndEp     int
	SuggestedSxxExx  string
	DefaultEdition   string
	IsCollision      bool
	TrackCount       int
	TracksSummary    string
	ISOSizeDisplay   string
}

// StatusResponse represents the JSON payload returned by GET /api/status.
type StatusResponse struct {
	DriveStatus         string `json:"drive_status"`
	RipperStatus        string `json:"ripper_status"`
	CombinedDriveStatus string `json:"combined_drive_status"`
	NFSStatus           string `json:"nfs_status"`
	NFSRetryPending     bool   `json:"nfs_retry_pending"`
	PendingTransfers    int    `json:"pending_transfers"`
	UnidentifiedCount   int    `json:"unidentified_count"`
}

// TagRequest represents a form or JSON request to confirm/tag or delete an item in /unidentified.
type TagRequest struct {
	ISOFilename   string `json:"iso_filename"`
	Action        string `json:"action"` // "tag" (default), "replace", or "delete"
	MediaType     string `json:"media_type"` // "movie" or "tv"
	TMDBID        int    `json:"tmdb_id"`
	Title         string `json:"title"`
	Year          int    `json:"year"`
	Season        int    `json:"season"`
	StartEpisode  int    `json:"start_episode"`
	EndEpisode    int    `json:"end_episode"`
	EditionSuffix string `json:"edition_suffix"`
	Overwrite     bool   `json:"overwrite"`
}

// NewServer constructs a Web UI Server and compiles its embedded html/template views.
func NewServer(
	poller DriveStatusProvider,
	ripWorker RipperStatusProvider,
	nfsWriter *nfs.Writer,
	tmdb *metadata.TMDBClient,
	histStore *history.Store,
	logBuf *history.LogBuffer,
	notifier *webhook.Notifier,
) *Server {
	funcs := template.FuncMap{
		"mulPct": func(v float64) float64 {
			return v * 100.0
		},
	}
	s := &Server{
		Poller:    poller,
		Ripper:    ripWorker,
		NFSWriter: nfsWriter,
		TMDB:      tmdb,
		History:   histStore,
		Logs:      logBuf,
		Webhook:   notifier,
		Logger:    log.Default(),
		tmpl:      template.Must(template.New("dashboard").Funcs(funcs).Parse(dashboardTemplateHTML)),
		mux:       http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/", s.handleDashboard)
	s.mux.HandleFunc("/unidentified", s.handleDashboard)
	s.mux.HandleFunc("/unidentified/tag", s.handleTagUnidentified)
	s.mux.HandleFunc("/unidentified/delete", s.handleDeleteUnidentified)
	s.mux.HandleFunc("/retry-transfer", s.handleRetryTransfer)

	s.mux.HandleFunc("/api/status", s.handleAPIStatus)
	s.mux.HandleFunc("/api/history", s.handleAPIHistory)
	s.mux.HandleFunc("/api/unidentified", s.handleAPIUnidentified)
	s.mux.HandleFunc("/api/unidentified/tag", s.handleTagUnidentified)
	s.mux.HandleFunc("/api/unidentified/delete", s.handleDeleteUnidentified)
	s.mux.HandleFunc("/api/retry-transfer", s.handleRetryTransfer)
	s.mux.HandleFunc("/api/tmdb/search", s.handleAPITMDBSearch)
}

// CurrentStatus computes the live optical drive, ripper, and NFS transfer queue status.
func (s *Server) CurrentStatus() StatusResponse {
	driveLabel := "Unknown"
	if s.Poller != nil {
		_, _, driveLabel = s.Poller.Status()
	}

	ripperLabel := ripper.StatusIdle
	if s.Ripper != nil {
		ripperLabel = s.Ripper.Status()
	}

	combined := driveLabel
	switch ripperLabel {
	case ripper.StatusWaitingForScratch:
		combined = "Waiting for Scratch Space"
	case ripper.StatusRippingStage1DVDBackup:
		combined = "Ripping Stage 1 (stage1_dvdbackup)"
	case ripper.StatusRippingStage1GenISO:
		combined = "Ripping Stage 1 (stage1_genisoimage)"
	case ripper.StatusRippingStage2DDRescue:
		combined = "Ripping Stage 2 (stage2_ddrescue)"
	default:
		if ripperLabel != "" && ripperLabel != ripper.StatusIdle {
			combined = ripperLabel
		}
	}

	nfsLabel := nfs.StatusIdle
	if s.NFSWriter != nil {
		nfsLabel = s.NFSWriter.Status()
	}

	pending := 0
	if s.QueueCountFunc != nil {
		pending = s.QueueCountFunc()
	}

	unidCount := 0
	if s.NFSWriter != nil {
		if items, err := s.NFSWriter.ListUnidentified(); err == nil {
			unidCount = len(items)
		}
	}

	return StatusResponse{
		DriveStatus:         driveLabel,
		RipperStatus:        ripperLabel,
		CombinedDriveStatus: combined,
		NFSStatus:           nfsLabel,
		NFSRetryPending:     nfsLabel == nfs.StatusNFSTransferPendingRetry,
		PendingTransfers:    pending,
		UnidentifiedCount:   unidCount,
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/unidentified" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := s.CurrentStatus()

	var unidViews []UnidentifiedView
	if s.NFSWriter != nil {
		sidecars, err := s.NFSWriter.ListUnidentified()
		if err != nil {
			s.logf("Warning: failed to list /unidentified: %v", err)
		}
		for _, sc := range sidecars {
			unidViews = append(unidViews, s.buildUnidentifiedView(sc))
		}
	}
	status.UnidentifiedCount = len(unidViews)

	var historyEntries []history.Entry
	if s.History != nil {
		historyEntries = s.History.Entries()
	}

	var logLines []string
	if s.Logs != nil {
		logLines = s.Logs.Lines()
	}

	data := struct {
		Status       StatusResponse
		Unidentified []UnidentifiedView
		History      []history.Entry
		Logs         []string
		FlashMsg     string
		ErrorMsg     string
	}{
		Status:       status,
		Unidentified: unidViews,
		History:      historyEntries,
		Logs:         logLines,
		FlashMsg:     strings.TrimSpace(r.URL.Query().Get("msg")),
		ErrorMsg:     strings.TrimSpace(r.URL.Query().Get("err")),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		s.logf("Error rendering dashboard template: %v", err)
	}
}

func (s *Server) buildUnidentifiedView(sc *metadata.ProbeSidecar) UnidentifiedView {
	mediaType := "movie"
	if sc.Classification == metadata.MediaTypeTVSeries || sc.Hints.Season > 0 {
		mediaType = "tv"
	}

	var defaultID, defaultYear int
	defaultTitle := sc.SanitizedQuery
	if len(sc.TMDBCandidates) > 0 {
		c := sc.TMDBCandidates[0]
		defaultID = c.ID
		defaultTitle = c.Title
		defaultYear = c.Year
		if c.MediaType == "tv" {
			mediaType = "tv"
		} else if c.MediaType == "movie" && sc.Classification != metadata.MediaTypeTVSeries {
			mediaType = "movie"
		}
	}
	if defaultYear == 0 && sc.Hints.Year > 0 {
		defaultYear = sc.Hints.Year
	}

	season := sc.Hints.Season
	if season <= 0 {
		season = 1
	}
	startEp := 1
	endEp := sc.Hints.EstimatedEpisodes
	if endEp < startEp {
		endEp = startEp
	}

	var epNums []int
	for e := startEp; e <= endEp; e++ {
		epNums = append(epNums, e)
	}
	suggestedSxxExx := formatSxxExxRange(season, startEp, endEp)

	isCollision := sc.Reason == metadata.ReasonTargetAlreadyExists
	defaultEdition := ""
	if isCollision {
		if sc.Hints.Disc > 1 {
			defaultEdition = fmt.Sprintf("Disc %d", sc.Hints.Disc)
		} else {
			defaultEdition = "Disc 2"
		}
	} else if sc.Hints.Disc > 1 && mediaType == "movie" {
		defaultEdition = fmt.Sprintf("Disc %d", sc.Hints.Disc)
	}

	var trackMins []string
	for _, sec := range sc.TracksSeconds {
		if sec >= 60 {
			trackMins = append(trackMins, fmt.Sprintf("%.0fm", sec/60.0))
		} else {
			trackMins = append(trackMins, fmt.Sprintf("%.0fs", sec))
		}
	}
	tracksSummary := "No tracks"
	if len(trackMins) > 0 {
		tracksSummary = strings.Join(trackMins, ", ")
	}

	isoSizeDisplay := ""
	if s.NFSWriter != nil && sc.ISOFilename != "" {
		isoPath := filepath.Join(s.NFSWriter.NFSRoot, "unidentified", filepath.Base(sc.ISOFilename))
		if info, err := os.Stat(isoPath); err == nil && !info.IsDir() {
			isoSizeDisplay = formatByteSize(info.Size())
		}
	}

	return UnidentifiedView{
		Sidecar:          sc,
		DefaultMediaType: mediaType,
		DefaultTMDBID:    defaultID,
		DefaultTitle:     defaultTitle,
		DefaultYear:      defaultYear,
		DefaultSeason:    season,
		DefaultStartEp:   startEp,
		DefaultEndEp:     endEp,
		SuggestedSxxExx:  suggestedSxxExx,
		DefaultEdition:   defaultEdition,
		IsCollision:      isCollision,
		TrackCount:       len(sc.TracksSeconds),
		TracksSummary:    tracksSummary,
		ISOSizeDisplay:   isoSizeDisplay,
	}
}

func formatSxxExxRange(season, startEp, endEp int) string {
	if season <= 0 {
		season = 1
	}
	if startEp <= 0 {
		startEp = 1
	}
	if endEp < startEp {
		endEp = startEp
	}
	var b strings.Builder
	fmt.Fprintf(&b, "S%02d", season)
	for ep := startEp; ep <= endEp; ep++ {
		fmt.Fprintf(&b, "E%02d", ep)
	}
	return b.String()
}

func formatByteSize(bytes int64) string {
	const gib = 1024 * 1024 * 1024
	const mib = 1024 * 1024
	if bytes >= gib {
		return fmt.Sprintf("%.2f GiB", float64(bytes)/float64(gib))
	}
	if bytes >= mib {
		return fmt.Sprintf("%.1f MiB", float64(bytes)/float64(mib))
	}
	return fmt.Sprintf("%d B", bytes)
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.CurrentStatus())
}

func (s *Server) handleAPIHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entries := []history.Entry{}
	if s.History != nil {
		entries = s.History.Entries()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
	})
}

func (s *Server) handleAPIUnidentified(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	items := []*metadata.ProbeSidecar{}
	if s.NFSWriter != nil {
		if list, err := s.NFSWriter.ListUnidentified(); err == nil && list != nil {
			items = list
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
	})
}

func (s *Server) handleRetryTransfer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.NFSWriter != nil {
		s.NFSWriter.TriggerRetry()
	}
	if wantsJSONResponse(r) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"message": "Triggered immediate NFS transfer retry",
		})
		return
	}
	http.Redirect(w, r, "/?msg=Triggered+immediate+NFS+transfer+retry", http.StatusSeeOther)
}

func (s *Server) handleAPITMDBSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	mediaType := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing query parameter q"})
		return
	}
	if s.TMDB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "TMDB client not configured"})
		return
	}

	var candidates []metadata.TMDBCandidate
	if mediaType == "tv" || mediaType == "tv_series" {
		results, err := s.TMDB.SearchTV(r.Context(), query)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		for _, item := range results {
			candidates = append(candidates, metadata.TMDBCandidate{
				ID:         item.ID,
				MediaType:  "tv",
				Title:      item.Name,
				Year:       item.Year(),
				Overview:   item.Overview,
				PosterPath: item.PosterPath,
			})
		}
	} else {
		results, err := s.TMDB.SearchMovies(r.Context(), query)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		for _, item := range results {
			candidates = append(candidates, metadata.TMDBCandidate{
				ID:         item.ID,
				MediaType:  "movie",
				Title:      item.Title,
				Year:       item.Year(),
				Overview:   item.Overview,
				PosterPath: item.PosterPath,
			})
		}
	}
	if candidates == nil {
		candidates = []metadata.TMDBCandidate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": candidates})
}

func (s *Server) handleDeleteUnidentified(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, err := parseTagRequest(r)
	if err != nil {
		s.respondError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	req.Action = "delete"
	s.executeTagAction(w, r, req)
}

func (s *Server) handleTagUnidentified(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, err := parseTagRequest(r)
	if err != nil {
		s.respondError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.executeTagAction(w, r, req)
}

func parseTagRequest(r *http.Request) (TagRequest, error) {
	var req TagRequest
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			return req, err
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return req, fmt.Errorf("invalid JSON body: %w", err)
		}
		return req, nil
	}

	if err := r.ParseForm(); err != nil {
		return req, fmt.Errorf("parse form: %w", err)
	}
	req.ISOFilename = strings.TrimSpace(r.FormValue("iso_filename"))
	req.Action = strings.TrimSpace(r.FormValue("action"))
	if req.Action == "" {
		req.Action = strings.TrimSpace(r.FormValue("resolution"))
	}
	req.MediaType = strings.TrimSpace(r.FormValue("media_type"))
	if v := strings.TrimSpace(r.FormValue("tmdb_id")); v != "" {
		req.TMDBID, _ = strconv.Atoi(v)
	}
	req.Title = strings.TrimSpace(r.FormValue("title"))
	if v := strings.TrimSpace(r.FormValue("year")); v != "" {
		req.Year, _ = strconv.Atoi(v)
	}
	if v := strings.TrimSpace(r.FormValue("season")); v != "" {
		req.Season, _ = strconv.Atoi(v)
	}
	if v := strings.TrimSpace(r.FormValue("start_episode")); v != "" {
		req.StartEpisode, _ = strconv.Atoi(v)
	}
	if v := strings.TrimSpace(r.FormValue("end_episode")); v != "" {
		req.EndEpisode, _ = strconv.Atoi(v)
	}
	req.EditionSuffix = strings.TrimSpace(r.FormValue("edition_suffix"))
	ov := strings.ToLower(strings.TrimSpace(r.FormValue("overwrite")))
	req.Overwrite = ov == "true" || ov == "1" || ov == "on" || ov == "yes"
	return req, nil
}

func (s *Server) executeTagAction(w http.ResponseWriter, r *http.Request, req TagRequest) {
	if s.NFSWriter == nil {
		s.respondError(w, r, http.StatusInternalServerError, "NFS writer is not configured")
		return
	}

	isoBase := filepath.Base(strings.TrimSpace(req.ISOFilename))
	if isoBase == "" || isoBase == "." || isoBase == ".." || isoBase == "/" {
		s.respondError(w, r, http.StatusBadRequest, "iso_filename is required")
		return
	}
	if strings.HasSuffix(isoBase, ".probe.json") {
		isoBase = strings.TrimSuffix(isoBase, ".probe.json") + ".iso"
	} else if !strings.HasSuffix(strings.ToLower(isoBase), ".iso") {
		isoBase = isoBase + ".iso"
	}

	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action == "delete" {
		if err := s.NFSWriter.DeleteUnidentified(isoBase); err != nil {
			s.respondError(w, r, http.StatusNotFound, err.Error())
			return
		}
		if s.History != nil {
			_ = s.History.UpdateByISOPath(isoBase, func(e *history.Entry) {
				if e.RawLabel == "" {
					e.RawLabel = isoBase
				}
				e.Status = history.StatusDeleted
				e.Reason = "deleted_via_webui"
			})
		}
		s.respondSuccess(w, r, fmt.Sprintf("Deleted unidentified disc %s", isoBase), "")
		return
	}

	if action == "replace" {
		req.Overwrite = true
	}

	// Read .probe.json sidecar if available to supply pre-filled defaults
	probePath := filepath.Join(s.NFSWriter.NFSRoot, "unidentified", strings.TrimSuffix(isoBase, filepath.Ext(isoBase))+".probe.json")
	sidecar, _ := metadata.ReadProbeSidecar(probePath)

	rawLabel := isoBase
	if sidecar != nil && strings.TrimSpace(sidecar.RawLabel) != "" {
		rawLabel = sidecar.RawLabel
	}

	mediaType := strings.ToLower(strings.TrimSpace(req.MediaType))
	if mediaType == "" && sidecar != nil {
		if sidecar.Classification == metadata.MediaTypeTVSeries || sidecar.Hints.Season > 0 {
			mediaType = "tv"
		} else {
			mediaType = "movie"
		}
	}
	if mediaType == "tv_series" || mediaType == "series" || mediaType == "show" {
		mediaType = "tv"
	}
	if mediaType != "tv" {
		mediaType = "movie"
	}

	tmdbID := req.TMDBID
	if tmdbID == 0 && sidecar != nil && len(sidecar.TMDBCandidates) > 0 {
		tmdbID = sidecar.TMDBCandidates[0].ID
	}

	if mediaType == "tv" {
		finalISOPath, showTitle, err := s.tagAsTVShow(r.Context(), isoBase, sidecar, tmdbID, req)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, nfs.ErrTargetAlreadyExists) {
				status = http.StatusConflict
			}
			s.respondError(w, r, status, err.Error())
			return
		}

		if s.History != nil {
			_ = s.History.UpdateByISOPath(isoBase, func(e *history.Entry) {
				if e.RawLabel == "" {
					e.RawLabel = rawLabel
				}
				e.Title = showTitle
				e.Classification = metadata.MediaTypeTVSeries
				e.Status = history.StatusCompleted
				e.ISOPath = finalISOPath
				e.Reason = ""
			})
		}
		if s.Webhook != nil {
			s.Webhook.SendAsync(s.Webhook.BuildCompletedPayload(rawLabel, metadata.MediaTypeTVSeries, "webui_tag", finalISOPath))
		}
		s.respondSuccess(w, r, fmt.Sprintf("Tagged TV Show -> %s", finalISOPath), finalISOPath)
		return
	}

	finalISOPath, movieTitle, err := s.tagAsMovie(r.Context(), isoBase, sidecar, tmdbID, req)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, nfs.ErrTargetAlreadyExists) {
			status = http.StatusConflict
		}
		s.respondError(w, r, status, err.Error())
		return
	}

	if s.History != nil {
		_ = s.History.UpdateByISOPath(isoBase, func(e *history.Entry) {
			if e.RawLabel == "" {
				e.RawLabel = rawLabel
			}
			e.Title = movieTitle
			e.Classification = metadata.MediaTypeMovie
			e.Status = history.StatusCompleted
			e.ISOPath = finalISOPath
			e.Reason = ""
		})
	}
	if s.Webhook != nil {
		s.Webhook.SendAsync(s.Webhook.BuildCompletedPayload(rawLabel, metadata.MediaTypeMovie, "webui_tag", finalISOPath))
	}
	s.respondSuccess(w, r, fmt.Sprintf("Tagged Movie -> %s", finalISOPath), finalISOPath)
}

func (s *Server) tagAsMovie(ctx context.Context, isoBase string, sidecar *metadata.ProbeSidecar, tmdbID int, req TagRequest) (string, string, error) {
	var details *metadata.TMDBMovieDetails
	if s.TMDB != nil && tmdbID > 0 {
		if d, err := s.TMDB.GetMovieDetails(ctx, tmdbID); err == nil && d != nil {
			details = d
		}
	}

	if details == nil {
		cand := findCandidate(sidecar, tmdbID, "movie")
		title := strings.TrimSpace(req.Title)
		year := req.Year
		overview := ""
		poster := ""
		if cand != nil {
			if title == "" {
				title = cand.Title
			}
			if year == 0 {
				year = cand.Year
			}
			overview = cand.Overview
			poster = cand.PosterPath
			if tmdbID == 0 {
				tmdbID = cand.ID
			}
		}
		if title == "" && sidecar != nil {
			title = strings.TrimSpace(sidecar.SanitizedQuery)
			if year == 0 {
				year = sidecar.Hints.Year
			}
		}
		if title == "" {
			return "", "", errors.New("movie title or valid TMDB ID is required")
		}
		details = &metadata.TMDBMovieDetails{
			ID:            tmdbID,
			Title:         title,
			OriginalTitle: title,
			ReleaseDate:   releaseDateFromYear(year),
			Overview:      overview,
			PosterPath:    poster,
		}
	} else {
		if strings.TrimSpace(req.Title) != "" {
			details.Title = strings.TrimSpace(req.Title)
		}
		if req.Year > 0 {
			details.ReleaseDate = releaseDateFromYear(req.Year)
		}
	}

	finalISOPath, err := s.NFSWriter.TagUnidentifiedAsMovie(isoBase, details, req.EditionSuffix, req.Overwrite)
	if err != nil {
		return "", "", err
	}
	displayTitle := metadata.FormatMovieBaseName(details.Title, details.Year(), req.EditionSuffix)
	return finalISOPath, displayTitle, nil
}

func (s *Server) tagAsTVShow(ctx context.Context, isoBase string, sidecar *metadata.ProbeSidecar, tmdbID int, req TagRequest) (string, string, error) {
	season := req.Season
	if season <= 0 && sidecar != nil && sidecar.Hints.Season > 0 {
		season = sidecar.Hints.Season
	}
	if season <= 0 {
		season = 1
	}

	startEp := req.StartEpisode
	if startEp <= 0 {
		startEp = 1
	}
	endEp := req.EndEpisode
	if endEp <= 0 {
		est := 1
		if sidecar != nil && sidecar.Hints.EstimatedEpisodes > 0 {
			est = sidecar.Hints.EstimatedEpisodes
		}
		endEp = startEp + est - 1
	}
	if endEp < startEp {
		endEp = startEp
	}

	var showDetails *metadata.TMDBTVDetails
	if s.TMDB != nil && tmdbID > 0 {
		if d, err := s.TMDB.GetTVDetails(ctx, tmdbID); err == nil && d != nil {
			showDetails = d
		}
	}

	if showDetails == nil {
		cand := findCandidate(sidecar, tmdbID, "tv")
		title := strings.TrimSpace(req.Title)
		year := req.Year
		overview := ""
		poster := ""
		if cand != nil {
			if title == "" {
				title = cand.Title
			}
			if year == 0 {
				year = cand.Year
			}
			overview = cand.Overview
			poster = cand.PosterPath
			if tmdbID == 0 {
				tmdbID = cand.ID
			}
		}
		if title == "" && sidecar != nil {
			title = strings.TrimSpace(sidecar.SanitizedQuery)
			if year == 0 {
				year = sidecar.Hints.Year
			}
		}
		if title == "" {
			return "", "", errors.New("TV show title or valid TMDB ID is required")
		}
		showDetails = &metadata.TMDBTVDetails{
			ID:           tmdbID,
			Name:         title,
			OriginalName: title,
			FirstAirDate: releaseDateFromYear(year),
			Overview:     overview,
			PosterPath:   poster,
		}
	} else {
		if strings.TrimSpace(req.Title) != "" {
			showDetails.Name = strings.TrimSpace(req.Title)
		}
		if req.Year > 0 {
			showDetails.FirstAirDate = releaseDateFromYear(req.Year)
		}
	}

	epByNumber := map[int]metadata.TMDBEpisode{}
	if s.TMDB != nil && showDetails.ID > 0 {
		if seasonDetails, err := s.TMDB.GetTVSeason(ctx, showDetails.ID, season); err == nil && seasonDetails != nil {
			for _, ep := range seasonDetails.Episodes {
				ep.SeasonNumber = season
				epByNumber[ep.EpisodeNumber] = ep
			}
		}
	}

	var episodes []metadata.TMDBEpisode
	var epNums []int
	for epNum := startEp; epNum <= endEp; epNum++ {
		epNums = append(epNums, epNum)
		if ep, ok := epByNumber[epNum]; ok {
			episodes = append(episodes, ep)
		} else {
			episodes = append(episodes, metadata.TMDBEpisode{
				SeasonNumber:  season,
				EpisodeNumber: epNum,
				Name:          fmt.Sprintf("Episode %d", epNum),
			})
		}
	}

	finalISOPath, err := s.NFSWriter.TagUnidentifiedAsTVShow(isoBase, showDetails, season, episodes, req.Overwrite)
	if err != nil {
		return "", "", err
	}
	displayTitle := metadata.FormatMultiEpisodeBaseName(showDetails.Name, season, epNums)
	return finalISOPath, displayTitle, nil
}

func findCandidate(sidecar *metadata.ProbeSidecar, tmdbID int, mediaType string) *metadata.TMDBCandidate {
	if sidecar == nil || len(sidecar.TMDBCandidates) == 0 {
		return nil
	}
	if tmdbID > 0 {
		for i := range sidecar.TMDBCandidates {
			if sidecar.TMDBCandidates[i].ID == tmdbID {
				return &sidecar.TMDBCandidates[i]
			}
		}
	}
	for i := range sidecar.TMDBCandidates {
		if strings.EqualFold(sidecar.TMDBCandidates[i].MediaType, mediaType) {
			return &sidecar.TMDBCandidates[i]
		}
	}
	return &sidecar.TMDBCandidates[0]
}

func releaseDateFromYear(year int) string {
	if year <= 0 {
		return ""
	}
	return fmt.Sprintf("%04d-01-01", year)
}

func (s *Server) respondSuccess(w http.ResponseWriter, r *http.Request, msg string, finalISOPath string) {
	if wantsJSONResponse(r) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"message":  msg,
			"iso_path": finalISOPath,
		})
		return
	}
	http.Redirect(w, r, "/?msg="+urlQueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) respondError(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	if wantsJSONResponse(r) {
		writeJSON(w, status, map[string]any{
			"ok":    false,
			"error": errMsg,
		})
		return
	}
	http.Redirect(w, r, "/?err="+urlQueryEscape(errMsg), http.StatusSeeOther)
}

func wantsJSONResponse(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	return strings.Contains(accept, "application/json") || strings.Contains(ct, "application/json")
}

func urlQueryEscape(s string) string {
	return strings.ReplaceAll(template.URLQueryEscaper(s), " ", "+")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
	}
}

var _ = drive.CDS_NO_DISC
var _ = time.Second
