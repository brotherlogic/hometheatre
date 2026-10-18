package webhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestWebhookNotificationsAllEvents(t *testing.T) {
	var mu sync.Mutex
	var received []Payload
	var contentTypes []string
	var methods []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		methods = append(methods, r.Method)
		contentTypes = append(contentTypes, r.Header.Get("Content-Type"))

		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("failed to decode webhook payload: %v", err)
		}
		received = append(received, p)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fixedTime := time.Date(2026, 10, 18, 15, 30, 0, 0, time.UTC)
	notifier := NewNotifier(srv.URL)
	notifier.Now = func() time.Time { return fixedTime }

	// 1. rip_unidentified (exact example from METADATA.md Section 5.5)
	pUnid := notifier.BuildUnidentifiedPayload(
		"OFFICE_S2_D1",
		"TV_SERIES",
		"stage1_dvdbackup",
		"/mnt/nfs/unidentified/OFFICE_S2_D1_20261018T153000Z.iso",
		"tv_series_requires_episode_confirmation",
		6,
	)
	notifier.SendAsync(pUnid)

	// 2. rip_completed
	pComp := notifier.BuildCompletedPayload(
		"THE_MATRIX_16X9",
		"MOVIE",
		"stage1_genisoimage",
		"/mnt/nfs/movies/The Matrix (1999)/The Matrix (1999).iso",
	)
	notifier.SendAsync(pComp)

	// 3. rip_failed
	pFail := notifier.BuildFailedPayload(
		"SCRATCHED_DISC",
		"UNIDENTIFIED",
		"stage2_ddrescue",
		"",
		"stage2_ddrescue_failed",
		0.82,
	)
	notifier.SendAsync(pFail)

	notifier.Wait()

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 3 {
		t.Fatalf("expected 3 webhook POSTs, got %d", len(received))
	}

	byEvent := map[string]Payload{}
	for i, p := range received {
		if methods[i] != http.MethodPost {
			t.Fatalf("expected POST method, got %s", methods[i])
		}
		if contentTypes[i] != "application/json" {
			t.Fatalf("expected Content-Type application/json, got %s", contentTypes[i])
		}
		if p.Text == "" || p.Text != p.Content {
			t.Fatalf("expected non-empty matching text and content fields, got text=%q content=%q", p.Text, p.Content)
		}
		if p.Timestamp != "2026-10-18T15:30:00Z" {
			t.Fatalf("expected timestamp 2026-10-18T15:30:00Z, got %q", p.Timestamp)
		}
		byEvent[p.Event] = p
	}

	gotUnid, ok := byEvent[EventRipUnidentified]
	if !ok {
		t.Fatalf("missing %s event", EventRipUnidentified)
	}
	wantUnidText := "[HomeTheatre] Disc needs tagging: OFFICE_S2_D1 (TV_SERIES, 6 episodes detected)"
	if gotUnid.Text != wantUnidText || gotUnid.Content != wantUnidText {
		t.Fatalf("got unidentified text=%q, want %q", gotUnid.Text, wantUnidText)
	}
	if gotUnid.RawLabel != "OFFICE_S2_D1" ||
		gotUnid.Classification != "TV_SERIES" ||
		gotUnid.Stage != "stage1_dvdbackup" ||
		gotUnid.ISOPath != "/mnt/nfs/unidentified/OFFICE_S2_D1_20261018T153000Z.iso" ||
		gotUnid.Reason != "tv_series_requires_episode_confirmation" {
		t.Fatalf("unexpected unidentified payload fields: %+v", gotUnid)
	}

	gotComp, ok := byEvent[EventRipCompleted]
	if !ok {
		t.Fatalf("missing %s event", EventRipCompleted)
	}
	if gotComp.RawLabel != "THE_MATRIX_16X9" ||
		gotComp.Classification != "MOVIE" ||
		gotComp.ISOPath != "/mnt/nfs/movies/The Matrix (1999)/The Matrix (1999).iso" {
		t.Fatalf("unexpected completed payload fields: %+v", gotComp)
	}

	gotFail, ok := byEvent[EventRipFailed]
	if !ok {
		t.Fatalf("missing %s event", EventRipFailed)
	}
	if gotFail.RawLabel != "SCRATCHED_DISC" ||
		gotFail.Stage != "stage2_ddrescue" ||
		gotFail.Reason != "stage2_ddrescue_failed" ||
		gotFail.RescuedRatio != 0.82 {
		t.Fatalf("unexpected failed payload fields: %+v", gotFail)
	}
}

func TestWebhookDisabledNoOp(t *testing.T) {
	notifier := NewNotifier("")
	if notifier.Enabled() {
		t.Fatalf("expected empty WEBHOOK_URL notifier to be disabled")
	}
	notifier.SendAsync(notifier.BuildCompletedPayload("TEST", "MOVIE", "stage1_dvdbackup", "/mnt/nfs/movies/Test.iso"))
	notifier.Wait()
}
