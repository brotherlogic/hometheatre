package metadata

import (
	"bytes"
	"context"
	"encoding/xml"
	"strings"
)

// CommandRunner executes an external CLI command and returns its output.
type CommandRunner func(ctx context.Context, env []string, name string, args ...string) (output string, err error)

// LSDVDOutput represents the XML structure emitted by `lsdvd -Ox <target>`.
type LSDVDOutput struct {
	XMLName      xml.Name     `xml:"lsdvd"`
	Device       string       `xml:"device"`
	Title        string       `xml:"title"`
	VMGID        string       `xml:"vmg_id"`
	ProviderID   string       `xml:"provider_id"`
	Tracks       []LSDVDTrack `xml:"track"`
	LongestTrack int          `xml:"longest_track"`
}

// LSDVDTrack represents a single <track> entry in `lsdvd -Ox` XML output.
type LSDVDTrack struct {
	Index  int     `xml:"ix"`
	Length float64 `xml:"length"` // Duration in seconds (%.3f)
	VTSID  string  `xml:"vts_id"`
}

// ProbeResult holds the extracted volume label and track durations from probing a device or ISO.
type ProbeResult struct {
	RawLabel      string
	LSDVDTitle    string
	TracksSeconds []float64
}

// ParseLSDVDXML sanitizes non-UTF8 bytes and bare ampersands in raw `lsdvd -Ox` output
// before unmarshaling the XML structure.
func ParseLSDVDXML(raw []byte) (*LSDVDOutput, error) {
	clean := bytes.ToValidUTF8(raw, []byte(""))
	clean = extractLSDVDElement(clean)
	clean = sanitizeBareAmpersands(clean)

	var out LSDVDOutput
	if err := xml.Unmarshal(clean, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// extractLSDVDElement isolates the <lsdvd>...</lsdvd> block if surrounding stderr messages
// (e.g. from libdvdcss/libdvdread when CombinedOutput is used) are present in the byte slice.
func extractLSDVDElement(b []byte) []byte {
	start := bytes.Index(b, []byte("<lsdvd"))
	end := bytes.LastIndex(b, []byte("</lsdvd>"))
	if start >= 0 && end >= start {
		return b[start : end+len("</lsdvd>")]
	}
	return b
}

// sanitizeBareAmpersands replaces any '&' that does not start a valid XML entity reference
// (&amp;, &lt;, &gt;, &apos;, &quot;, &#123;, &#x1F;) with "&amp;".
func sanitizeBareAmpersands(b []byte) []byte {
	if !bytes.ContainsRune(b, '&') {
		return b
	}
	var out bytes.Buffer
	out.Grow(len(b) + 16)

	for i := 0; i < len(b); i++ {
		if b[i] == '&' && !hasValidXMLEntityPrefix(b[i+1:]) {
			out.WriteString("&amp;")
			continue
		}
		out.WriteByte(b[i])
	}
	return out.Bytes()
}

func hasValidXMLEntityPrefix(rest []byte) bool {
	semi := bytes.IndexByte(rest, ';')
	if semi <= 0 || semi > 12 {
		return false
	}
	entity := string(rest[:semi])
	switch entity {
	case "amp", "lt", "gt", "apos", "quot":
		return true
	}
	if strings.HasPrefix(entity, "#x") || strings.HasPrefix(entity, "#X") {
		if len(entity) < 3 {
			return false
		}
		for i := 2; i < len(entity); i++ {
			c := entity[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
		return true
	}
	if strings.HasPrefix(entity, "#") {
		if len(entity) < 2 {
			return false
		}
		for i := 1; i < len(entity); i++ {
			c := entity[i]
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	return false
}

// ProbeTarget extracts the DVD volume label (blkid primary, lsdvd -Ox <title> fallback)
// and track durations (lsdvd -Ox) from a block device (/dev/sr0) or .iso file.
func ProbeTarget(ctx context.Context, runner CommandRunner, dvdcssEnv []string, target string) (*ProbeResult, error) {
	res := &ProbeResult{
		TracksSeconds: []float64{},
	}

	// 1. Primary label extraction via blkid -o value -s LABEL <target>
	if out, err := runner(ctx, nil, "blkid", "-o", "value", "-s", "LABEL", target); err == nil {
		label := cleanVolumeLabel(out)
		if label != "" {
			res.RawLabel = label
		}
	}

	// 2. Track lengths + fallback <title> via lsdvd -Ox <target> (never pass -v or -x)
	lsdvdOut, lsdvdErr := runner(ctx, dvdcssEnv, "lsdvd", "-Ox", target)
	if strings.TrimSpace(lsdvdOut) != "" {
		parsed, parseErr := ParseLSDVDXML([]byte(lsdvdOut))
		if parseErr == nil {
			res.LSDVDTitle = cleanVolumeLabel(parsed.Title)
			if res.RawLabel == "" && res.LSDVDTitle != "" {
				res.RawLabel = res.LSDVDTitle
			}
			for _, tr := range parsed.Tracks {
				if tr.Length > 0 {
					res.TracksSeconds = append(res.TracksSeconds, tr.Length)
				}
			}
			return res, nil
		}
		if lsdvdErr == nil {
			lsdvdErr = parseErr
		}
	}

	return res, lsdvdErr
}

// NeedsTrackReprobe returns true if the initial probe failed to extract any valid tracks.
func NeedsTrackReprobe(tracksSeconds []float64) bool {
	return len(tracksSeconds) == 0
}

// NeedsLabelReprobe returns true if the raw volume label is empty or "unknown".
func NeedsLabelReprobe(rawLabel string) bool {
	return cleanVolumeLabel(rawLabel) == ""
}

func cleanVolumeLabel(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || strings.EqualFold(trimmed, "unknown") {
		return ""
	}
	return trimmed
}
