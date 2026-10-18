package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// MaxEntries is the maximum number of rip records persisted in <SCRATCH_DIR>/history.json
	// per METADATA.md Section 5.5.
	MaxEntries = 100

	// DefaultMaxLogLines is the default ring buffer capacity for live daemon logs shown in the Web UI.
	DefaultMaxLogLines = 200

	StatusCompleted    = "completed"
	StatusUnidentified = "unidentified"
	StatusFailed       = "failed"
	StatusDeleted      = "deleted"
)

// Entry represents a single rip history record persisted in <SCRATCH_DIR>/history.json.
type Entry struct {
	JobID          string  `json:"job_id"`
	Timestamp      string  `json:"timestamp"`
	RawLabel       string  `json:"raw_label"`
	Title          string  `json:"title,omitempty"`
	Classification string  `json:"classification,omitempty"`
	Status         string  `json:"status"`
	Stage          string  `json:"stage,omitempty"`
	ISOPath        string  `json:"iso_path,omitempty"`
	Reason         string  `json:"reason,omitempty"`
	RescuedRatio   float64 `json:"rescued_ratio,omitempty"`
	Log            string  `json:"log,omitempty"`
}

// Store manages persistent rip history in <SCRATCH_DIR>/history.json.
type Store struct {
	ScratchDir string
	MaxLimit   int
	Now        func() time.Time

	mu      sync.RWMutex
	entries []Entry
}

// NewStore initializes a Store backed by <scratchDir>/history.json, loading any existing entries on disk.
func NewStore(scratchDir string) (*Store, error) {
	s := &Store{
		ScratchDir: scratchDir,
		MaxLimit:   MaxEntries,
		Now:        time.Now,
		entries:    []Entry{},
	}
	if err := s.load(); err != nil && !os.IsNotExist(err) {
		return s, err
	}
	return s, nil
}

// Path returns the path to <SCRATCH_DIR>/history.json.
func (s *Store) Path() string {
	return filepath.Join(s.ScratchDir, "history.json")
}

// Entries returns a snapshot of the persisted history records (newest first).
func (s *Store) Entries() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Entry, len(s.entries))
	copy(out, s.entries)
	return out
}

// Record prepends a new Entry to the history list, trims to MaxEntries (100),
// and atomically persists <SCRATCH_DIR>/history.json via history.json.tmp + os.Rename.
func (s *Store) Record(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(e.Timestamp) == "" {
		e.Timestamp = s.nowUTC().Format(time.RFC3339)
	}

	s.entries = append([]Entry{e}, s.entries...)
	limit := s.maxLimit()
	if len(s.entries) > limit {
		s.entries = s.entries[:limit]
	}
	return s.saveLocked()
}

// UpdateByISOPath updates the most recent history entry matching oldISOPath (or base filename)
// in-place, or prepends a new entry if no existing entry matches, then atomically persists history.json.
func (s *Store) UpdateByISOPath(oldISOPath string, updateFn func(*Entry)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldBase := filepath.Base(strings.TrimSpace(oldISOPath))
	for i := range s.entries {
		if s.entries[i].ISOPath == oldISOPath || (oldBase != "" && filepath.Base(s.entries[i].ISOPath) == oldBase) {
			updateFn(&s.entries[i])
			if strings.TrimSpace(s.entries[i].Timestamp) == "" {
				s.entries[i].Timestamp = s.nowUTC().Format(time.RFC3339)
			}
			return s.saveLocked()
		}
	}

	var e Entry
	e.ISOPath = oldISOPath
	e.Timestamp = s.nowUTC().Format(time.RFC3339)
	updateFn(&e)
	s.entries = append([]Entry{e}, s.entries...)
	limit := s.maxLimit()
	if len(s.entries) > limit {
		s.entries = s.entries[:limit]
	}
	return s.saveLocked()
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.Path())
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		s.entries = []Entry{}
		return nil
	}
	var parsed []Entry
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("unmarshal %s: %w", s.Path(), err)
	}
	limit := s.maxLimit()
	if len(parsed) > limit {
		parsed = parsed[:limit]
	}
	s.entries = parsed
	return nil
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(s.ScratchDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.ScratchDir, err)
	}

	records := s.entries
	if records == nil {
		records = []Entry{}
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal history.json: %w", err)
	}
	data = append(data, '\n')

	tmpPath := filepath.Join(s.ScratchDir, "history.json.tmp")
	finalPath := s.Path()

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", tmpPath, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("fsync %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, finalPath, err)
	}
	return nil
}

func (s *Store) maxLimit() int {
	if s.MaxLimit > 0 {
		return s.MaxLimit
	}
	return MaxEntries
}

func (s *Store) nowUTC() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// LogBuffer is a thread-safe in-memory ring buffer implementing io.Writer to capture
// recent daemon log lines for display in the Web UI.
type LogBuffer struct {
	mu       sync.RWMutex
	maxLines int
	lines    []string
}

// NewLogBuffer creates a LogBuffer retaining at most maxLines lines.
func NewLogBuffer(maxLines int) *LogBuffer {
	if maxLines <= 0 {
		maxLines = DefaultMaxLogLines
	}
	return &LogBuffer{
		maxLines: maxLines,
		lines:    []string{},
	}
}

// Write splits incoming log bytes into lines and appends them to the ring buffer.
func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	raw := strings.TrimRight(string(p), "\r\n")
	if raw == "" {
		return len(p), nil
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		b.lines = append(b.lines, line)
	}
	if len(b.lines) > b.maxLines {
		b.lines = b.lines[len(b.lines)-b.maxLines:]
	}
	return len(p), nil
}

// Lines returns a copy of the buffered log lines in chronological order.
func (b *LogBuffer) Lines() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}
