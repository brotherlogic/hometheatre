package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/brotherlogic/hometheatre/internal/drive"
	"github.com/brotherlogic/hometheatre/internal/ripper"
)

func main() {
	devicePath := envOrDefault("OPTICAL_DEVICE", "/dev/sr0")
	scratchDir := envOrDefault("SCRATCH_DIR", "/scratch")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	transferQueue := make(chan *ripper.Job, 64)

	worker := ripper.NewWorker(scratchDir, devicePath)
	worker.HasPendingTransfers = func() bool {
		return len(transferQueue) > 0
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

	log.Printf("Starting HomeTheatre optical drive poller on %s (scratch=%s)", devicePath, scratchDir)
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
