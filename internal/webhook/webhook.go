package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	EventRipCompleted    = "rip_completed"
	EventRipUnidentified = "rip_unidentified"
	EventRipFailed       = "rip_failed"

	// DefaultTimeout is the 10s HTTP timeout for webhook POST notifications (METADATA.md Section 5.5).
	DefaultTimeout = 10 * time.Second
)

// Payload represents the JSON webhook body sent to WEBHOOK_URL per METADATA.md Section 5.5.
type Payload struct {
	Event          string  `json:"event"`
	Text           string  `json:"text"`
	Content        string  `json:"content"`
	Timestamp      string  `json:"timestamp"`
	RawLabel       string  `json:"raw_label"`
	Classification string  `json:"classification"`
	Stage          string  `json:"stage"`
	ISOPath        string  `json:"iso_path"`
	Reason         string  `json:"reason"`
	RescuedRatio   float64 `json:"rescued_ratio,omitempty"`
}

// Notifier dispatches JSON webhook notifications to WEBHOOK_URL when configured.
type Notifier struct {
	WebhookURL string
	HTTPClient *http.Client
	Now        func() time.Time
	Logger     *log.Logger

	wg sync.WaitGroup
}

// NewNotifier creates a Notifier for the given webhookURL (no-op when webhookURL is empty).
func NewNotifier(webhookURL string) *Notifier {
	return &Notifier{
		WebhookURL: strings.TrimSpace(webhookURL),
		HTTPClient: &http.Client{Timeout: DefaultTimeout},
		Now:        time.Now,
		Logger:     log.Default(),
	}
}

// Enabled returns true when a non-empty WEBHOOK_URL is configured.
func (n *Notifier) Enabled() bool {
	return n != nil && strings.TrimSpace(n.WebhookURL) != ""
}

// Wait blocks until all in-flight asynchronous webhook requests have finished.
func (n *Notifier) Wait() {
	if n == nil {
		return
	}
	n.wg.Wait()
}

// Send dispatches a synchronous HTTP POST with Content-Type: application/json to WebhookURL.
func (n *Notifier) Send(ctx context.Context, p Payload) error {
	if !n.Enabled() {
		return nil
	}

	p = n.normalizePayload(p, 0)
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(n.WebhookURL), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := n.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post webhook: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// SendAsync dispatches the webhook notification asynchronously with a 10s timeout.
func (n *Notifier) SendAsync(p Payload) {
	if !n.Enabled() {
		return
	}
	p = n.normalizePayload(p, 0)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
		defer cancel()
		if err := n.Send(ctx, p); err != nil && n.Logger != nil {
			n.Logger.Printf("Warning: webhook notification (%s) failed: %v", p.Event, err)
		}
	}()
}

// BuildCompletedPayload constructs a Payload for the rip_completed event.
func (n *Notifier) BuildCompletedPayload(rawLabel, classification, stage, isoPath string) Payload {
	p := Payload{
		Event:          EventRipCompleted,
		RawLabel:       rawLabel,
		Classification: classification,
		Stage:          stage,
		ISOPath:        isoPath,
	}
	return n.normalizePayload(p, 0)
}

// BuildUnidentifiedPayload constructs a Payload for the rip_unidentified event.
func (n *Notifier) BuildUnidentifiedPayload(rawLabel, classification, stage, isoPath, reason string, estimatedEpisodes int) Payload {
	p := Payload{
		Event:          EventRipUnidentified,
		RawLabel:       rawLabel,
		Classification: classification,
		Stage:          stage,
		ISOPath:        isoPath,
		Reason:         reason,
	}
	return n.normalizePayload(p, estimatedEpisodes)
}

// BuildFailedPayload constructs a Payload for the rip_failed event.
func (n *Notifier) BuildFailedPayload(rawLabel, classification, stage, isoPath, reason string, rescuedRatio float64) Payload {
	p := Payload{
		Event:          EventRipFailed,
		RawLabel:       rawLabel,
		Classification: classification,
		Stage:          stage,
		ISOPath:        isoPath,
		Reason:         reason,
		RescuedRatio:   rescuedRatio,
	}
	return n.normalizePayload(p, 0)
}

func (n *Notifier) normalizePayload(p Payload, estimatedEpisodes int) Payload {
	if strings.TrimSpace(p.Timestamp) == "" {
		p.Timestamp = n.nowUTC().Format(time.RFC3339)
	}
	summary := strings.TrimSpace(p.Text)
	if summary == "" {
		summary = strings.TrimSpace(p.Content)
	}
	if summary == "" {
		summary = formatSummary(p, estimatedEpisodes)
	}
	if strings.TrimSpace(p.Text) == "" {
		p.Text = summary
	}
	if strings.TrimSpace(p.Content) == "" {
		p.Content = summary
	}
	return p
}

func formatSummary(p Payload, estimatedEpisodes int) string {
	label := strings.TrimSpace(p.RawLabel)
	if label == "" {
		label = "UNKNOWN_DISC"
	}
	switch p.Event {
	case EventRipCompleted:
		return fmt.Sprintf("[HomeTheatre] Rip completed: %s -> %s", label, p.ISOPath)
	case EventRipUnidentified:
		if p.Classification == "TV_SERIES" && estimatedEpisodes > 0 {
			return fmt.Sprintf("[HomeTheatre] Disc needs tagging: %s (%s, %d episodes detected)", label, p.Classification, estimatedEpisodes)
		}
		if p.Classification != "" && p.Reason != "" {
			return fmt.Sprintf("[HomeTheatre] Disc needs tagging: %s (%s, %s)", label, p.Classification, p.Reason)
		}
		return fmt.Sprintf("[HomeTheatre] Disc needs tagging: %s", label)
	case EventRipFailed:
		if p.Stage != "" && p.Reason != "" {
			return fmt.Sprintf("[HomeTheatre] Rip failed: %s (stage: %s, reason: %s)", label, p.Stage, p.Reason)
		}
		if p.Reason != "" {
			return fmt.Sprintf("[HomeTheatre] Rip failed: %s (%s)", label, p.Reason)
		}
		return fmt.Sprintf("[HomeTheatre] Rip failed: %s", label)
	default:
		return fmt.Sprintf("[HomeTheatre] %s: %s", p.Event, label)
	}
}

func (n *Notifier) nowUTC() time.Time {
	if n != nil && n.Now != nil {
		return n.Now().UTC()
	}
	return time.Now().UTC()
}
