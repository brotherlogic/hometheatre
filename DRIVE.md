# Optical Drive Detection, Non-Intrusive Polling & Docker Passthrough (`DRIVE.md`)

This document captures the technical design and verified implementation details for optical drive detection, non-intrusive disc polling in Go, Docker device passthrough, and host auto-detection in `install.sh` (resolving [Issue #2](https://github.com/brotherlogic/hometheatre/issues/2)).

---

## 1. Non-Intrusive Optical Drive Polling in Go (`/dev/sr0`)

### 1.1 Why `ioctl(CDROM_DRIVE_STATUS)` with `O_RDONLY | O_NONBLOCK`
Polling an optical drive on Linux requires querying the drive controller without triggering unwanted mechanical side effects:
- **Opening without `O_NONBLOCK` is intrusive:** Calling `open("/dev/sr0", O_RDONLY)` without `O_NONBLOCK` causes the Linux `sr` / `cdrom` driver to block while the drive spins up, fail with `ENOMEDIUM` when empty, or **automatically close an open tray** if `CDROM_AUTO_CLOSE` is active.
- **Shelling out to `blkid /dev/sr0` on a loop is intrusive:** `blkid` opens the block device normally and attempts sector reads for filesystem superblocks, which can retract an open tray while a user is placing a disc or spin the drive unnecessarily.
- **Relying on `/sys/class/block/sr0/events` is brittle in containers:** In-kernel media polling (`events_poll_msecs`) is often `-1` (disabled) on minimal headless servers unless host `udev`/`udisks2` rules enable it.

Instead, the Go daemon uses `golang.org/x/sys/unix` (or `syscall`) to:
1. Open `/dev/sr0` with `unix.O_RDONLY | unix.O_NONBLOCK`.
2. Issue `ioctl(fd, CDROM_DRIVE_STATUS, CDSL_CURRENT)` (`0x5326`, slot `0`).
3. Close `fd` immediately after reading the status integer.

This queries the drive's MMC status (`TEST UNIT READY` / `GET EVENT STATUS NOTIFICATION`) in microseconds without seeking, spinning up an empty spindle, or closing an open tray.

### 1.2 Kernel `linux/cdrom.h` Constants & Go Poller Sketch

```go
package drive

import (
	"golang.org/x/sys/unix"
)

const (
	// ioctl request codes from <linux/cdrom.h>
	CDROM_DRIVE_STATUS = 0x5326
	CDROM_DISC_STATUS  = 0x5327

	// Slot selector
	CDSL_CURRENT = 0

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
)

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
```

### 1.3 Poller State Machine & Edge Cases
- **Poll Interval:** `2s` ticker.
- **Initial Startup Behavior (`UNKNOWN` -> `CDS_DISC_OK`):**
  - The poller initializes its previous state to `UNKNOWN` (`-1`).
  - If a DVD is already in the drive when the container starts (e.g., inserted prior to boot or left in after a container restart), the first poll observes `UNKNOWN -> CDS_DISC_OK` and immediately triggers the rip-and-eject pipeline.
- **Single-Trigger Latch per Insertion Cycle:**
  - A rip is only triggered on a state transition from `!= CDS_DISC_OK` to `CDS_DISC_OK`.
  - Once triggered, the disc is marked as handled for the current insertion cycle. Even if `eject` fails or a slimline drive tray remains latched after popping open slightly, the daemon will **not** re-rip the same disc until the poller first observes a non-ready state (`CDS_TRAY_OPEN`, `CDS_NO_DISC`, or `CDS_DRIVE_NOT_READY`).
- **Media Validation:**
  - When `CDS_DISC_OK` is reached, the daemon checks that the media is data/DVD (`discStatus != CDS_AUDIO` and `blkid` reports `udf` or `iso9660`) before starting `dvdbackup`. Audio CDs or blank discs are logged as unsupported and ejected cleanly.
- **Web UI Live Status Mapping:**
  - `CDS_NO_DISC` -> `Empty`
  - `CDS_TRAY_OPEN` -> `Tray Open`
  - `CDS_DRIVE_NOT_READY` -> `Spinning Up / Reading TOC`
  - `CDS_DISC_OK` -> `Disc Ready` / `Ripping (<Stage>)`

---

## 2. Docker Passthrough, Permissions & Capabilities

### 2.1 Device Mapping (`/dev/sr*` + `/dev/sg*`)
- Optical drives on Linux expose two device nodes:
  1. **Block device (`/dev/sr0`, major `11`):** Used by `blkid`, `lsdvd`, `dvdbackup`, `ddrescue`, and `eject`.
  2. **Generic SCSI character device (`/dev/sg*`, major `21`):** Used for low-level SCSI pass-through (`SG_IO`) and CSS authentication fallbacks on certain drives.
- Because the drive is permanently attached to the host, `docker-compose.yml` maps both explicit devices discovered by `install.sh` via `.env`:
  ```yaml
  devices:
    - "${OPTICAL_SR_DEVICE:-/dev/sr0}:/dev/sr0"
    - "${OPTICAL_SG_DEVICE:-/dev/sg0}:${OPTICAL_SG_DEVICE:-/dev/sg0}"
  ```
  Inside the container, the block device is normalized to `/dev/sr0` while the matching `/dev/sg*` path is preserved.

### 2.2 Container User, Group & Linux Capabilities
- **Process User:** The container runs as `root`.
  - On Linux, granting `cap_add: [SYS_RAWIO]` in Docker Compose only populates the effective/permitted `CAP_SYS_RAWIO` capability automatically for `root` processes (non-root processes require `setcap` file capabilities on every child binary).
  - Running as `root` inside the isolated appliance container guarantees zero permission friction for `DVD_AUTH` ioctls, `SG_IO` SCSI pass-through commands, `blkid -p`, and `eject`.
  - Per [`DOCKER.md`](DOCKER.md), `PUID` / `PGID` environment variables are applied via thread-scoped `unix.Setfsuid(puid)` / `unix.Setfsgid(pgid)` (`WithNFSUser`) whenever creating or writing `.iso`, `.nfo`, and `.probe.json` files on `/mnt/nfs`, ensuring compatibility with `root_squash` NAS exports while the daemon process runs as `root`.
- **Group & Capabilities in `docker-compose.yml`:**
  ```yaml
  group_add:
    - "${CDROM_GID:-24}" # Debian/Ubuntu default cdrom group GID is 24
  cap_add:
    - SYS_RAWIO          # Required for unfiltered SG_IO / raw SCSI MMC & CSS key exchange
  ```

---

## 3. Host Auto-Detection in `install.sh`

### 3.1 Ensuring the `sg` Kernel Module is Loaded
On Debian and Ubuntu hosts, the generic SCSI driver (`sg`) is not always loaded by default at boot, which causes `/dev/sg*` and `/sys/class/block/sr*/device/scsi_generic` to be missing even when `/dev/sr0` exists.

`install.sh` must:
1. Run `modprobe sg`.
2. Persist it across reboots by writing `sg` to `/etc/modules-load.d/hometheatre-sg.conf`.

### 3.2 Discovering Matching `/dev/sr*` and `/dev/sg*` Pairs via `sysfs`
Rather than fragile regex parsing of `lsscsi` stdout, `install.sh` resolves the exact kernel relationship from `/sys/class/block/sr*`:

```bash
discover_optical_drives() {
    modprobe sg 2>/dev/null || true
    for sr_sys in /sys/class/block/sr*; do
        [ -e "$sr_sys" ] || continue
        sr_name=$(basename "$sr_sys")
        sr_dev="/dev/${sr_name}"

        sg_sys=$( echo "$sr_sys"/device/scsi_generic/sg* )
        sg_name=$(basename "$sg_sys")
        sg_dev="/dev/${sg_name}"

        vendor=$(tr -d '[:space:]' < "$sr_sys/device/vendor" 2>/dev/null || echo "Unknown")
        model=$(sed 's/[[:space:]]*$//' "$sr_sys/device/model" 2>/dev/null || echo "Optical Drive")

        echo "${sr_dev}|${sg_dev}|${vendor} ${model}"
    done
}
```

### 3.3 Handling 0, 1, or Multiple Optical Drives
1. **Exactly 1 drive detected:**
   - Automatically selects the `/dev/srX` and `/dev/sgY` pair, logs the detected vendor/model (e.g. `Detected optical drive: ASUS DRW-24B1ST (/dev/sr0 -> /dev/sg1)`), and writes `OPTICAL_SR_DEVICE` and `OPTICAL_SG_DEVICE` into `.env`.
2. **Multiple drives detected (`/dev/sr0`, `/dev/sr1`, ...):**
   - Displays a numbered list of detected drives (`vendor model (/dev/srX + /dev/sgY)`) and prompts the user to select which drive the container should use.
3. **0 drives detected:**
   - If `OPTICAL_SR_DEVICE` and `OPTICAL_SG_DEVICE` are not already explicitly set in `.env`, `install.sh` exits with a clear error message instructing the user to connect an optical drive (or specify the device override in `.env`).
