package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brotherlogic/hometheatre/internal/drive"
	"github.com/brotherlogic/hometheatre/internal/history"
	"github.com/brotherlogic/hometheatre/internal/metadata"
	"github.com/brotherlogic/hometheatre/internal/nfs"
	"github.com/brotherlogic/hometheatre/internal/ripper"
	"github.com/brotherlogic/hometheatre/internal/webhook"
	"github.com/brotherlogic/hometheatre/internal/webui"
)

func main() {
	devicePath := envOrDefault("OPTICAL_DEVICE", "/dev/sr0")
	scratchDir := envOrDefault("SCRATCH_DIR", "/scratch")
	nfsRoot := envOrDefault("NFS_ROOT", "/mnt/nfs")
	puid := envIntOrDefault("PUID", 1000)
	pgid := envIntOrDefault("PGID", 1000)
	tmdbAPIKey := os.Getenv("TMDB_API_KEY")
	webhookURL := os.Getenv("WEBHOOK_URL")
	webPort := envOrDefault("WEB_PORT", "8080")
	webAddr := envOrDefault("WEB_ADDR", ":"+strings.TrimPrefix(webPort, ":"))

	logBuf := history.NewLogBuffer(history.DefaultMaxLogLines)
	log.SetOutput(io.MultiWriter(os.Stderr, logBuf))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	histStore, err := history.NewStore(scratchDir)
	if err != nil {
		log.Printf("Warning: failed to load history.json from %s: %v", scratchDir, err)
	}
	notifier := webhook.NewNotifier(webhookURL)
	defer notifier.Wait()

	transferQueue := make(chan *ripper.Job, 64)

	tmdbClient := metadata.NewTMDBClient(tmdbAPIKey)
	nfsWriter := nfs.NewWriter(nfsRoot, puid, pgid, tmdbClient)
	nfsWriter.DVDCSSCache = filepath.Join(scratchDir, ".dvdcss")
	nfsWriter.OnTransferCompleted = func(outcome nfs.TransferOutcome) {
		if outcome.AutoFiled {
			log.Printf("Auto-filed movie %s -> %s", outcome.JobID, outcome.FinalISOPath)
			_ = histStore.Record(history.Entry{
				JobID:          outcome.JobID,
				RawLabel:       outcome.RawLabel,
				Title:          outcome.Title,
				Classification: outcome.Classification,
				Status:         history.StatusCompleted,
				Stage:          outcome.Stage,
				ISOPath:        outcome.FinalISOPath,
				RescuedRatio:   outcome.RescuedRatio,
			})
			notifier.SendAsync(notifier.BuildCompletedPayload(
				outcome.RawLabel,
				outcome.Classification,
				outcome.Stage,
				outcome.FinalISOPath,
			))
		} else {
			log.Printf("Moved %s (%s, reason=%s) -> %s",
				outcome.JobID, outcome.Classification, outcome.Reason, outcome.FinalISOPath)
			_ = histStore.Record(history.Entry{
				JobID:          outcome.JobID,
				RawLabel:       outcome.RawLabel,
				Classification: outcome.Classification,
				Status:         history.StatusUnidentified,
				Stage:          outcome.Stage,
				ISOPath:        outcome.FinalISOPath,
				Reason:         outcome.Reason,
				RescuedRatio:   outcome.RescuedRatio,
			})
			notifier.SendAsync(notifier.BuildUnidentifiedPayload(
				outcome.RawLabel,
				outcome.Classification,
				outcome.Stage,
				outcome.FinalISOPath,
				outcome.Reason,
				outcome.EstimatedEpisodes,
			))
		}
	}
	nfsWriter.OnTransferRetry = func(ev nfs.TransferRetryEvent) {
		if ev.Attempt == 1 {
			notifier.SendAsync(notifier.BuildFailedPayload(
				ev.RawLabel,
				"",
				"nfs_transfer_retry",
				"",
				ev.Err.Error(),
				0,
			))
		}
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case job := <-transferQueue:
				if job == nil {
					continue
				}
				if _, err := nfsWriter.ProcessJobWithRetry(ctx, job); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("Transfer exited for job %s: %v", job.JobID, err)
				}
			}
		}
	}()

	worker := ripper.NewWorker(scratchDir, devicePath)
	worker.HasPendingTransfers = func() bool {
		return len(transferQueue) > 0 || nfsWriter.Status() != nfs.StatusIdle
	}
	worker.OnJobFinalized = func(job *ripper.Job) {
		log.Printf("ISO finalized for job %s (label=%q, stage=%s, iso=%s); enqueued for TMDB + NFS transfer",
			job.JobID, job.RawLabel, job.Stage, job.ISOPath)
		select {
		case transferQueue <- job:
		default:
			log.Printf("Warning: transfer queue full; job %s remains on scratch for reconciliation", job.JobID)
		}
	}
	worker.OnRipFailed = func(ev ripper.FailureEvent) {
		log.Printf("Rip failed (job=%s, label=%q, stage=%s, reason=%s, rescued=%.2f%%): %v",
			ev.JobID, ev.RawLabel, ev.Stage, ev.Reason, ev.RescuedRatio*100, ev.Err)
		logTail := ev.StderrTail
		if logTail == "" && ev.Err != nil {
			logTail = ev.Err.Error()
		}
		_ = histStore.Record(history.Entry{
			JobID:        ev.JobID,
			RawLabel:     ev.RawLabel,
			Status:       history.StatusFailed,
			Stage:        ev.Stage,
			Reason:       ev.Reason,
			RescuedRatio: ev.RescuedRatio,
			Log:          logTail,
		})
		notifier.SendAsync(notifier.BuildFailedPayload(
			ev.RawLabel,
			"",
			ev.Stage,
			"",
			ev.Reason,
			ev.RescuedRatio,
		))
	}

	recovered, err := ripper.ReconcileScratch(scratchDir, worker.OnJobFinalized)
	if err != nil {
		log.Printf("Warning: failed to reconcile scratch dir %s: %v", scratchDir, err)
	} else if len(recovered) > 0 {
		log.Printf("Recovered %d finalized ISO job(s) from %s", len(recovered), scratchDir)
	}

	poller := drive.NewPoller(devicePath, func(ctx context.Context, dev string) error {
		_, err := worker.RipDisc(ctx, dev)
		return err
	})

	uiServer := webui.NewServer(poller, worker, nfsWriter, tmdbClient, histStore, logBuf, notifier)
	uiServer.QueueCountFunc = func() int {
		return len(transferQueue)
	}

	httpSrv := &http.Server{
		Addr:              webAddr,
		Handler:           uiServer,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("Starting HomeTheatre Web UI on %s", webAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Web UI server error: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("Starting HomeTheatre optical drive poller on %s (scratch=%s, nfs=%s, puid=%d, pgid=%d)",
		devicePath, scratchDir, nfsRoot, puid, pgid)
	if err := poller.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("Poller exited with error: %v", err)
	}
}

func envOrDefault(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(val)
	if err != nil {
		return fallback
	}
	return parsed
}
