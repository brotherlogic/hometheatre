package drive

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// ioctl request codes from <linux/cdrom.h>
	CDROM_SET_OPTIONS   = 0x5320
	CDROM_CLEAR_OPTIONS = 0x5321
	CDROM_DRIVE_STATUS  = 0x5326
	CDROM_DISC_STATUS   = 0x5327
	CDROM_LOCKDOOR      = 0x5329

	// CD-ROM driver option flags from <linux/cdrom.h>
	CDO_AUTO_CLOSE = 0x0001
	CDO_LOCK       = 0x0008

	// Slot selector
	CDSL_CURRENT = 0

	// Initial poller state before first observation
	CDS_UNKNOWN = -1

	// Drive status return values (CDROM_DRIVE_STATUS)
	CDS_NO_INFO         = 0
	CDS_NO_DISC         = 1
	CDS_TRAY_OPEN       = 2
	CDS_DRIVE_NOT_READY = 3
	CDS_DISC_OK         = 4

	// Disc type return values (CDROM_DISC_STATUS)
	CDS_AUDIO  = 100
	CDS_DATA_1 = 101
	CDS_DATA_2 = 102
	CDS_XA_2_1 = 103
	CDS_XA_2_2 = 104
	CDS_MIXED  = 105

	// DefaultPollInterval is the non-intrusive optical drive poll interval (2s).
	DefaultPollInterval = 2 * time.Second
)

// QueryDriveStatus opens devicePath with O_RDONLY|O_NONBLOCK and queries
// CDROM_DRIVE_STATUS and (when CDS_DISC_OK) CDROM_DISC_STATUS via ioctl.
func QueryDriveStatus(devicePath string) (driveStatus int, discStatus int, err error) {
	fd, err := unix.Open(devicePath, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return CDS_NO_INFO, CDS_NO_INFO, err
	}
	defer unix.Close(fd)

	// Pass CDSL_CURRENT (0) as the 3rd syscall argument
	r1, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(CDROM_DRIVE_STATUS), uintptr(CDSL_CURRENT))
	if errno != 0 {
		return CDS_NO_INFO, CDS_NO_INFO, errno
	}
	driveStatus = int(r1)

	if driveStatus == CDS_DISC_OK {
		r2, _, errno2 := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(CDROM_DISC_STATUS), uintptr(CDSL_CURRENT))
		if errno2 == 0 {
			discStatus = int(r2)
		}
	}
	return driveStatus, discStatus, nil
}

// DriveStatusString maps a CDROM_DRIVE_STATUS constant to its Web UI display label.
func DriveStatusString(driveStatus int) string {
	switch driveStatus {
	case CDS_NO_DISC:
		return "Empty"
	case CDS_TRAY_OPEN:
		return "Tray Open"
	case CDS_DRIVE_NOT_READY:
		return "Spinning Up / Reading TOC"
	case CDS_DISC_OK:
		return "Disc Ready"
	default:
		return "Unknown"
	}
}

// StatusQueryFunc queries the optical drive status for a device path.
type StatusQueryFunc func(devicePath string) (driveStatus int, discStatus int, err error)

// WaitForDiscReady polls queryFn (defaulting to QueryDriveStatus) until the drive
// reports CDROM_DRIVE_STATUS == CDS_DISC_OK, or until timeout elapses / ctx is canceled.
func WaitForDiscReady(ctx context.Context, devicePath string, queryFn StatusQueryFunc, timeout, pollInterval time.Duration) (int, error) {
	if queryFn == nil {
		queryFn = QueryDriveStatus
	}
	if pollInterval <= 0 {
		pollInterval = 250 * time.Millisecond
	}

	deadline := time.Now().Add(timeout)
	for {
		driveStatus, _, err := queryFn(devicePath)
		if err == nil && driveStatus == CDS_DISC_OK {
			return CDS_DISC_OK, nil
		}

		if timeout <= 0 || time.Now().After(deadline) {
			if err != nil {
				return driveStatus, fmt.Errorf("drive %s not ready (status=%s): %w", devicePath, DriveStatusString(driveStatus), err)
			}
			return driveStatus, fmt.Errorf("drive %s not ready: status=%s (%d)", devicePath, DriveStatusString(driveStatus), driveStatus)
		}

		wait := pollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return driveStatus, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// FSProbeFunc returns the filesystem type (e.g. "udf", "iso9660") detected on devicePath.
type FSProbeFunc func(ctx context.Context, devicePath string) (string, error)

// EjectFunc ejects the optical drive tray for devicePath.
type EjectFunc func(ctx context.Context, devicePath string) error

// DiscReadyHandler is invoked once per insertion cycle when a valid data/DVD disc is ready.
type DiscReadyHandler func(ctx context.Context, devicePath string) error

// ProbeFilesystemType runs `blkid -o value -s TYPE <devicePath>` to inspect the disc filesystem.
func ProbeFilesystemType(ctx context.Context, devicePath string) (string, error) {
	out, err := exec.CommandContext(ctx, "blkid", "-o", "value", "-s", "TYPE", devicePath).Output()
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(string(out))), nil
}

// PrepareForEject opens devicePath with O_RDONLY|O_NONBLOCK, clears CDO_AUTO_CLOSE
// (so concurrent host udev/blkid opens without O_NONBLOCK cannot automatically pull
// an open motorized tray back in via CDROMCLOSETRAY), and unlocks the drive door
// (CDROM_LOCKDOOR = 0). Errors are ignored as best-effort.
func PrepareForEject(devicePath string) {
	fd, err := unix.Open(devicePath, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)

	_, _, _ = unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(CDROM_CLEAR_OPTIONS), uintptr(CDO_AUTO_CLOSE))
	_, _, _ = unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(CDROM_LOCKDOOR), 0)
}

// Eject runs `eject <devicePath>` to open the optical drive tray.
func Eject(ctx context.Context, devicePath string) error {
	PrepareForEject(devicePath)
	out, err := exec.CommandContext(ctx, "eject", devicePath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("eject %s failed: %w (output: %s)", devicePath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// IsSupportedFilesystem returns true if the blkid TYPE string indicates UDF or ISO9660 DVD media.
func IsSupportedFilesystem(fsType string) bool {
	fs := strings.ToLower(strings.TrimSpace(fsType))
	for _, token := range strings.Fields(fs) {
		if token == "udf" || token == "iso9660" {
			return true
		}
	}
	return fs == "udf" || fs == "iso9660"
}

// Poller monitors an optical drive device node for disc insertions and triggers
// the ripping pipeline once per insertion cycle.
type Poller struct {
	DevicePath   string
	PollInterval time.Duration
	QueryStatus  StatusQueryFunc
	ProbeFS      FSProbeFunc
	EjectDisc    EjectFunc
	OnDiscReady  DiscReadyHandler
	Logger       *log.Logger

	mu          sync.RWMutex
	prevStatus  int
	discStatus  int
	latched     bool
	statusLabel string
}

// NewPoller creates a Poller initialized with prevStatus = CDS_UNKNOWN (-1) so
// that a disc already present at container startup triggers immediately on the first poll.
func NewPoller(devicePath string, onDiscReady DiscReadyHandler) *Poller {
	return &Poller{
		DevicePath:   devicePath,
		PollInterval: DefaultPollInterval,
		QueryStatus:  QueryDriveStatus,
		ProbeFS:      ProbeFilesystemType,
		EjectDisc:    Eject,
		OnDiscReady:  onDiscReady,
		Logger:       log.Default(),
		prevStatus:   CDS_UNKNOWN,
		statusLabel:  DriveStatusString(CDS_UNKNOWN),
	}
}

// Status returns the current raw drive status constant, whether the current insertion
// cycle is latched, and the human-readable status label.
func (p *Poller) Status() (driveStatus int, latched bool, label string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.prevStatus, p.latched, p.statusLabel
}

// Step performs a single poll iteration. It is exported to allow deterministic unit testing.
func (p *Poller) Step(ctx context.Context) error {
	queryFn := p.QueryStatus
	if queryFn == nil {
		queryFn = QueryDriveStatus
	}

	driveStatus, discStatus, err := queryFn(p.DevicePath)
	if err != nil {
		return err
	}

	p.mu.Lock()
	previous := p.prevStatus
	wasLatched := p.latched

	switch driveStatus {
	case CDS_NO_DISC, CDS_TRAY_OPEN, CDS_DRIVE_NOT_READY:
		// Any non-ready state resets the single-trigger insertion latch.
		p.latched = false
		p.prevStatus = driveStatus
		p.discStatus = discStatus
		p.statusLabel = DriveStatusString(driveStatus)
		p.mu.Unlock()
		return nil

	case CDS_DISC_OK:
		p.prevStatus = CDS_DISC_OK
		p.discStatus = discStatus
		p.statusLabel = DriveStatusString(CDS_DISC_OK)

		// Single-trigger latch: only trigger on transition from != CDS_DISC_OK to CDS_DISC_OK
		// when not already latched for this insertion cycle.
		if wasLatched || previous == CDS_DISC_OK {
			p.mu.Unlock()
			return nil
		}
		p.latched = true
		p.mu.Unlock()

		return p.handleDiscInsertion(ctx, discStatus)

	default:
		p.prevStatus = driveStatus
		p.discStatus = discStatus
		p.statusLabel = DriveStatusString(driveStatus)
		p.mu.Unlock()
		return nil
	}
}

func (p *Poller) handleDiscInsertion(ctx context.Context, discStatus int) error {
	// 1. Reject audio CDs immediately.
	if discStatus == CDS_AUDIO {
		p.logf("Unsupported media in %s: audio CD detected (CDS_AUDIO); ejecting cleanly", p.DevicePath)
		p.ejectBestEffort(ctx)
		return nil
	}

	// 2. Validate that blkid reports udf or iso9660 before starting dvdbackup.
	probeFn := p.ProbeFS
	if probeFn == nil {
		probeFn = ProbeFilesystemType
	}
	fsType, err := probeFn(ctx, p.DevicePath)
	if err != nil || !IsSupportedFilesystem(fsType) {
		p.logf("Unsupported or blank media in %s (discStatus=%d, fsType=%q, err=%v); ejecting cleanly",
			p.DevicePath, discStatus, fsType, err)
		p.ejectBestEffort(ctx)
		return nil
	}

	p.logf("Valid DVD media detected in %s (fsType=%s); starting rip pipeline", p.DevicePath, fsType)
	if p.OnDiscReady != nil {
		return p.OnDiscReady(ctx, p.DevicePath)
	}
	return nil
}

func (p *Poller) ejectBestEffort(ctx context.Context) {
	ejectFn := p.EjectDisc
	if ejectFn == nil {
		ejectFn = Eject
	}
	if err := ejectFn(ctx, p.DevicePath); err != nil {
		p.logf("Warning: failed to eject %s: %v", p.DevicePath, err)
	}
}

func (p *Poller) logf(format string, args ...any) {
	if p.Logger != nil {
		p.Logger.Printf(format, args...)
	}
}

// Run starts the polling loop until ctx is canceled.
func (p *Poller) Run(ctx context.Context) error {
	interval := p.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}

	// Perform initial poll immediately on startup so UNKNOWN -> CDS_DISC_OK triggers without delay.
	_ = p.Step(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_ = p.Step(ctx)
		}
	}
}
