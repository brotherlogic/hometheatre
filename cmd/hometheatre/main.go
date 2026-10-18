package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/brotherlogic/hometheatre/internal/drive"
	"github.com/brotherlogic/hometheatre/internal/metadata"
	"github.com/brotherlogic/hometheatre/internal/nfs"
	"github.com/brotherlogic/hometheatre/internal/ripper"
)

func main() {
	devicePath := envOrDefault("OPTICAL_DEVICE", "/dev/sr0")
	scratchDir := envOrDefault("SCRATCH_DIR", "/scratch")
	nfsRoot := envOrDefault("NFS_ROOT", "/mnt/nfs")
	puid := envIntOrDefault("PUID", 1000)
	pgid := envIntOrDefault("PGID", 1000)
	tmdbAPIKey := os.Getenv("TMDB_API_KEY")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	transferQueue := make(chan *ripper.Job, 64)

	tmdbClient := metadata.NewTMDBClient(tmdbAPIKey)
	nfsWriter := nfs.NewWriter(nfsRoot, puid, pgid, tmdbClient)
	nfsWriter.DVDCSSCache = filepath.Join(scratchDir, ".dvdcss")
	nfsWriter.OnTransferCompleted = func(outcome nfs.TransferOutcome) {
		if outcome.AutoFiled {
			log.Printf("Auto-filed movie %s -> %s", outcome.JobID, outcome.FinalISOPath)
		} else {
			log.Printf("Moved %s (%s, reason=%s) -> %s",
				outcome.JobID, outcome.Classification, outcome.Reason, outcome.FinalISOPath)
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
