package board

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Attachment states.
const (
	AttachmentReady   = "ready"
	AttachmentHeld    = "held"
	AttachmentDropped = "dropped"
)

// Attachment kinds.
const (
	MediaImage  = "image"
	MediaMotion = "moving image"
)

// Attachment is one image or short moving image a post can show. Posts name
// it by Ref: the hash of its transformed bytes, or of the original while it
// is held. A released attachment keeps its Ref, so posts that name it need no
// edit.
type Attachment struct {
	Ref          string    `json:"ref"`
	OriginalHash string    `json:"original_hash"`
	OutputHash   string    `json:"output_hash,omitempty"`
	Name         string    `json:"name,omitempty"`
	State        string    `json:"state"`
	Reason       string    `json:"reason,omitempty"`
	Kind         string    `json:"kind,omitempty"`
	Type         string    `json:"type,omitempty"`
	Width        int       `json:"width,omitempty"`
	Height       int       `json:"height,omitempty"`
	DurationMS   int       `json:"duration_ms,omitempty"`
	Bytes        int64     `json:"bytes,omitempty"`
	Checks       []string  `json:"checks,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// MediaConfig sets the ingest pipeline's tools and caps
// (docs/crew-board.md, Media).
type MediaConfig struct {
	// Dir holds blobs/, quarantine/ and scratch/.
	Dir             string
	FFmpeg, FFprobe string
	// Sandbox wraps every decoder run, for example bubblewrap with no network
	// and only the scratch directory writable. The scratch directory is bound
	// at /work inside it. Empty runs decoders directly.
	Sandbox func(scratch string) []string
	// Scanner is a signature scan run on the original and the result, given
	// the file path last: exit 0 is clean, 1 is a hit. Empty skips the scan,
	// and the attachment records that it was not scanned.
	Scanner []string
	Timeout time.Duration

	StillPixels   int64
	StillBytes    int64
	MotionBytes   int64
	MotionSeconds float64
	PostBytes     int64
	UploadBytes   int64
}

// DefaultMediaConfig is the agreed caps with ffmpeg in bubblewrap.
func DefaultMediaConfig(dir string) MediaConfig {
	return MediaConfig{Dir: dir, FFmpeg: "ffmpeg", FFprobe: "ffprobe", Sandbox: Bubblewrap, Timeout: 90 * time.Second,
		StillPixels: 20_000_000, StillBytes: 2 << 20, MotionBytes: 10 << 20, MotionSeconds: 60, PostBytes: 50 << 20, UploadBytes: 100 << 20}
}

// Bubblewrap runs a decoder with no network, a read-only system, only the
// scratch directory writable, and CPU and memory limits.
func Bubblewrap(scratch string) []string {
	return []string{"bwrap", "--ro-bind", "/usr", "/usr", "--ro-bind", "/lib", "/lib", "--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind-try", "/etc/fonts", "/etc/fonts", "--symlink", "usr/bin", "/bin", "--proc", "/proc", "--dev", "/dev",
		"--bind", scratch, "/work", "--chdir", "/work", "--unshare-all", "--die-with-parent", "--new-session",
		"--", "prlimit", "--cpu=120", "--as=4294967296", "--"}
}

// Ingester turns uploads into attachments: it judges the type by content,
// refuses what is not media, and re-encodes the rest so that only known-safe
// bytes are kept.
type Ingester struct {
	cfg MediaConfig
	now func() time.Time
}

func NewIngester(cfg MediaConfig, now func() time.Time) (*Ingester, error) {
	for _, sub := range []string{"blobs", "quarantine", "scratch"} {
		if err := os.MkdirAll(filepath.Join(cfg.Dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	return &Ingester{cfg: cfg, now: now}, nil
}

// Media refusal codes.
const (
	CodeMediaTypeRefused  ErrorCode = "media_type_refused"
	CodeMediaTypeMismatch ErrorCode = "media_type_mismatch"
	CodeMediaTooLarge     ErrorCode = "media_too_large"
	CodeAttachmentUnknown ErrorCode = "attachment_unknown"
	CodeAttachmentHeld    ErrorCode = "attachment_held"
)

const convertHint = "only images and short moving images can be attached; convert documents to Markdown (with images if needed) and post that instead"

// sniff names a file's real type from its first bytes.
func sniff(data []byte) string {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	trimmed := bytes.TrimLeft(bytes.TrimPrefix(head, []byte("\xef\xbb\xbf")), " \t\r\n")
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		if bytes.Contains(data[:min(len(data), 1<<16)], []byte("acTL")) {
			return "apng"
		}
		return "png"
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "gif"
	case len(data) > 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		if len(data) > 21 && string(data[12:16]) == "VP8X" && data[20]&0x02 != 0 {
			return "webp-animated"
		}
		return "webp"
	case len(data) > 12 && string(data[4:8]) == "ftyp":
		brand := string(data[8:12])
		if brand == "avif" || brand == "avis" {
			return "avif"
		}
		return "mp4"
	case bytes.HasPrefix(data, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return "webm"
	case bytes.HasPrefix(data, []byte("%PDF")):
		return "pdf"
	case bytes.HasPrefix(data, []byte{0xD0, 0xCF, 0x11, 0xE0}):
		return "office"
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		if bytes.Contains(head, []byte("[Content_Types].xml")) || bytes.Contains(head, []byte("word/")) || bytes.Contains(head, []byte("xl/")) || bytes.Contains(head, []byte("ppt/")) {
			return "office"
		}
		return "archive"
	case bytes.HasPrefix(data, []byte{0x1F, 0x8B}), bytes.HasPrefix(data, []byte("7z\xbc\xaf\x27\x1c")), bytes.HasPrefix(data, []byte("Rar!")):
		return "archive"
	case bytes.HasPrefix(data, []byte("MZ")), bytes.HasPrefix(data, []byte("\x7fELF")), bytes.HasPrefix(data, []byte("#!")):
		return "executable"
	case bytes.HasPrefix(trimmed, []byte("<svg")), bytes.HasPrefix(trimmed, []byte("<?xml")) && bytes.Contains(head, []byte("<svg")):
		return "svg"
	}
	return "unknown"
}

// extensionType is the type a file name claims, or "" for none it knows.
var extensionType = map[string]string{".png": "png", ".apng": "apng", ".jpg": "jpeg", ".jpeg": "jpeg", ".gif": "gif", ".webp": "webp", ".avif": "avif",
	".svg": "svg", ".mp4": "mp4", ".m4v": "mp4", ".mov": "mp4", ".webm": "webm", ".pdf": "pdf", ".doc": "office", ".docx": "office",
	".xls": "office", ".xlsx": "office", ".ppt": "office", ".pptx": "office", ".zip": "archive", ".exe": "executable"}

func compatible(claimed, real string) bool {
	switch {
	case claimed == real:
		return true
	case claimed == "png" && real == "apng", claimed == "apng" && real == "png":
		return true
	case claimed == "webp" && real == "webp-animated":
		return true
	}
	return false
}

// polyglot reports content that makes a media file also something else.
func polyglot(data []byte, kind string) string {
	tail := data[max(0, len(data)-65536):]
	switch {
	case bytes.Contains(tail, []byte("PK\x05\x06")):
		return "it also holds a ZIP archive"
	case bytes.Contains(data[1:min(len(data), 1<<20)], []byte("%PDF-")):
		return "it also holds a PDF"
	case kind != "svg" && (bytes.Contains(bytes.ToLower(data[:min(len(data), 1<<20)]), []byte("<script")) || bytes.Contains(bytes.ToLower(data[:min(len(data), 1<<20)]), []byte("<html"))):
		return "it also holds HTML or script"
	}
	return ""
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// mediaError is a check that failed in a way that holds the attachment
// instead of refusing it.
type mediaError struct {
	check, reason string
}

func (e *mediaError) Error() string { return e.check + ": " + e.reason }

// Skip names checks a person's release lets through.
type Skip struct {
	Scan    bool
	Decoder bool
}

// Ingest runs the pipeline on one upload. A refusal is an Error; a failed
// check returns a held attachment (with the original kept in quarantine), and
// success a ready one whose transformed bytes are in blobs/.
func (in *Ingester) Ingest(ctx context.Context, name string, data []byte, skip Skip) (Attachment, error) {
	if in.cfg.UploadBytes > 0 && int64(len(data)) > in.cfg.UploadBytes {
		return Attachment{}, refuse(CodeMediaTooLarge, "uploads are at most %d MB", in.cfg.UploadBytes>>20)
	}
	kind := sniff(data)
	switch kind {
	case "pdf", "office", "archive", "executable", "unknown":
		return Attachment{}, refuse(CodeMediaTypeRefused, "this is %s; %s", describeType(kind), convertHint)
	case "webp-animated":
		return Attachment{}, refuse(CodeMediaTypeRefused, "animated WebP cannot be decoded on this site yet; send it as a GIF or an MP4")
	}
	if claimed, ok := extensionType[strings.ToLower(filepath.Ext(name))]; ok && !compatible(claimed, kind) {
		return Attachment{}, refuse(CodeMediaTypeMismatch, "the name says %s but the bytes are %s", describeType(claimed), describeType(kind))
	}
	if what := polyglot(data, kind); what != "" {
		return Attachment{}, refuse(CodeMediaTypeRefused, "the file is %s, but %s", describeType(kind), what)
	}
	original := digest(data)
	a := Attachment{OriginalHash: original, Name: filepath.Base(name), CreatedAt: in.now().UTC()}
	scratch, err := os.MkdirTemp(filepath.Join(in.cfg.Dir, "scratch"), "ingest-")
	if err != nil {
		return Attachment{}, err
	}
	defer os.RemoveAll(scratch)
	if err := os.WriteFile(filepath.Join(scratch, "in"), data, 0o600); err != nil {
		return Attachment{}, err
	}
	out, err := in.transform(ctx, scratch, kind, &a, skip)
	if err == nil {
		err = in.scan(ctx, filepath.Join(scratch, "in"), filepath.Join(scratch, out), &a, skip)
	}
	var held *mediaError
	if errors.As(err, &held) {
		a.Ref, a.State, a.Reason = original, AttachmentHeld, held.Error()
		if werr := os.WriteFile(in.quarantinePath(original), data, 0o600); werr != nil {
			return Attachment{}, werr
		}
		return a, nil
	}
	if err != nil {
		return Attachment{}, err
	}
	result, err := os.ReadFile(filepath.Join(scratch, out))
	if err != nil {
		return Attachment{}, err
	}
	a.OutputHash, a.Bytes, a.State = digest(result), int64(len(result)), AttachmentReady
	a.Ref = a.OutputHash
	path := in.BlobPath(a)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Attachment{}, err
	}
	if err := os.WriteFile(path, result, 0o600); err != nil {
		return Attachment{}, err
	}
	_ = os.Remove(in.quarantinePath(original))
	return a, nil
}

func describeType(kind string) string {
	return map[string]string{"pdf": "a PDF", "office": "an Office document", "archive": "an archive", "executable": "a program or script",
		"unknown": "not a recognised image or video", "png": "PNG", "apng": "APNG", "jpeg": "JPEG", "gif": "GIF", "webp": "WebP",
		"webp-animated": "animated WebP", "avif": "AVIF", "svg": "SVG", "mp4": "MP4", "webm": "WebM"}[kind]
}

// BlobPath is where a ready attachment's bytes live.
func (in *Ingester) BlobPath(a Attachment) string {
	ext := ".webp"
	if a.Type == "video/mp4" {
		ext = ".mp4"
	}
	return filepath.Join(in.cfg.Dir, "blobs", "sha256", a.OutputHash[:2], a.OutputHash+ext)
}

func (in *Ingester) quarantinePath(original string) string {
	return filepath.Join(in.cfg.Dir, "quarantine", original)
}

// Quarantined returns a held attachment's original bytes.
func (in *Ingester) Quarantined(a Attachment) ([]byte, error) {
	return os.ReadFile(in.quarantinePath(a.OriginalHash))
}

// Discard removes a held attachment's original.
func (in *Ingester) Discard(a Attachment) error {
	err := os.Remove(in.quarantinePath(a.OriginalHash))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

type probe struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Packets   string `json:"nb_read_packets"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// run executes a decoder inside the sandbox with the scratch directory at
// /work (or at its real path without a sandbox).
func (in *Ingester) run(ctx context.Context, scratch string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	work := scratch
	var command []string
	if in.cfg.Sandbox != nil {
		work = "/work"
		command = in.cfg.Sandbox(scratch)
	}
	for _, a := range args {
		command = append(command, strings.ReplaceAll(a, "{work}", work))
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = scratch
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, &mediaError{check: "decoder", reason: "timed out after " + timeout.String()}
	}
	if err != nil {
		return nil, &mediaError{check: "decoder", reason: "could not decode it: " + firstLineOf(stderr.String(), err.Error())}
	}
	return stdout.Bytes(), nil
}

func firstLineOf(text, fallback string) string {
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return fallback
}

// transform probes the original, enforces the caps, and re-encodes it: still
// images to WebP, moving images to H.264 MP4. It returns the output's name
// in the scratch directory.
func (in *Ingester) transform(ctx context.Context, scratch, kind string, a *Attachment, skip Skip) (string, error) {
	timeout := in.cfg.Timeout
	if skip.Decoder {
		timeout *= 4
	}
	input := []string{"-i", "{work}/in"}
	if kind == "svg" {
		input = []string{"-f", "svg_pipe", "-i", "{work}/in"}
	}
	raw, err := in.run(ctx, scratch, timeout, append(append([]string{in.cfg.FFprobe, "-v", "error", "-count_packets"}, inputOnly(input)...),
		"-show_entries", "stream=codec_type,width,height,nb_read_packets:format=duration", "-of", "json")...)
	if err != nil {
		return "", err
	}
	var p probe
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", &mediaError{check: "decoder", reason: "unreadable probe"}
	}
	width, height, frames := 0, 0, 0
	for _, s := range p.Streams {
		if s.CodecType == "video" {
			width, height = s.Width, s.Height
			frames, _ = strconv.Atoi(s.Packets)
			break
		}
	}
	if width == 0 && kind != "svg" {
		return "", &mediaError{check: "decoder", reason: "no picture in it"}
	}
	duration, _ := strconv.ParseFloat(p.Format.Duration, 64)
	motion := kind == "mp4" || kind == "webm" || (kind == "gif" || kind == "apng") && frames > 1
	if !motion {
		if pixels := int64(width) * int64(height); in.cfg.StillPixels > 0 && pixels > in.cfg.StillPixels {
			return "", refuse(CodeMediaTooLarge, "the image is %d×%d, over %d megapixels; scale it down first", width, height, in.cfg.StillPixels/1_000_000)
		}
		return in.still(ctx, scratch, input, timeout, a)
	}
	if in.cfg.MotionSeconds > 0 && duration > in.cfg.MotionSeconds+0.5 {
		return "", refuse(CodeMediaTooLarge, "the clip runs %.0f s; moving images are at most %.0f s", duration, in.cfg.MotionSeconds)
	}
	a.DurationMS = int(duration * 1000)
	return in.motion(ctx, scratch, input, timeout, a)
}

func inputOnly(input []string) []string {
	// ffprobe takes the file without -i.
	var out []string
	for i := 0; i < len(input); i++ {
		if input[i] == "-i" {
			continue
		}
		out = append(out, input[i])
	}
	return out
}

func (in *Ingester) still(ctx context.Context, scratch string, input []string, timeout time.Duration, a *Attachment) (string, error) {
	for _, attempt := range []struct {
		quality string
		edge    int
	}{{"85", 4096}, {"70", 4096}, {"60", 2048}} {
		args := append([]string{in.cfg.FFmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-threads", "2"}, input...)
		args = append(args, "-map_metadata", "-1", "-frames:v", "1",
			"-vf", fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", attempt.edge, attempt.edge),
			"-c:v", "libwebp", "-quality", attempt.quality, "-compression_level", "4", "{work}/out.webp")
		if _, err := in.run(ctx, scratch, timeout, args...); err != nil {
			return "", err
		}
		info, err := os.Stat(filepath.Join(scratch, "out.webp"))
		if err != nil {
			return "", &mediaError{check: "decoder", reason: "no output"}
		}
		if in.cfg.StillBytes <= 0 || info.Size() <= in.cfg.StillBytes {
			a.Kind, a.Type = MediaImage, "image/webp"
			return "out.webp", in.measure(ctx, scratch, "out.webp", timeout, a)
		}
	}
	return "", refuse(CodeMediaTooLarge, "the image stays over %d MB even re-encoded; send a smaller one", in.cfg.StillBytes>>20)
}

func (in *Ingester) motion(ctx context.Context, scratch string, input []string, timeout time.Duration, a *Attachment) (string, error) {
	for _, crf := range []string{"26", "32"} {
		args := append([]string{in.cfg.FFmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-threads", "2"}, input...)
		args = append(args, "-map_metadata", "-1", "-t", strconv.FormatFloat(in.cfg.MotionSeconds+0.5, 'f', 1, 64),
			"-vf", "scale='min(1920,iw)':'min(1080,ih)':force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2",
			"-c:v", "libx264", "-preset", "veryfast", "-crf", crf, "-pix_fmt", "yuv420p", "-movflags", "+faststart",
			"-c:a", "aac", "-b:a", "96k", "-ac", "2", "{work}/out.mp4")
		if _, err := in.run(ctx, scratch, timeout, args...); err != nil {
			return "", err
		}
		info, err := os.Stat(filepath.Join(scratch, "out.mp4"))
		if err != nil {
			return "", &mediaError{check: "decoder", reason: "no output"}
		}
		if in.cfg.MotionBytes <= 0 || info.Size() <= in.cfg.MotionBytes {
			a.Kind, a.Type = MediaMotion, "video/mp4"
			return "out.mp4", in.measure(ctx, scratch, "out.mp4", timeout, a)
		}
	}
	return "", refuse(CodeMediaTooLarge, "the clip stays over %d MB even re-encoded; send a shorter or smaller one", in.cfg.MotionBytes>>20)
}

// measure records the output's size in pixels.
func (in *Ingester) measure(ctx context.Context, scratch, out string, timeout time.Duration, a *Attachment) error {
	raw, err := in.run(ctx, scratch, timeout, in.cfg.FFprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "json", "{work}/"+out)
	if err != nil {
		return err
	}
	var p probe
	if json.Unmarshal(raw, &p) == nil && len(p.Streams) > 0 {
		a.Width, a.Height = p.Streams[0].Width, p.Streams[0].Height
	}
	return nil
}

// scan runs the signature scan on the original and the result. A hit holds
// the attachment; with no scanner configured the attachment says so.
func (in *Ingester) scan(ctx context.Context, original, output string, a *Attachment, skip Skip) error {
	if len(in.cfg.Scanner) == 0 {
		a.Checks = append(a.Checks, "re-encoded; not signature-scanned (no scanner on this site)")
		return nil
	}
	if skip.Scan {
		a.Checks = append(a.Checks, "re-encoded; signature scan skipped by a person's release")
		return nil
	}
	for _, path := range []string{original, output} {
		ctx, cancel := context.WithTimeout(ctx, in.cfg.Timeout)
		cmd := exec.CommandContext(ctx, in.cfg.Scanner[0], append(append([]string{}, in.cfg.Scanner[1:]...), path)...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		cancel()
		var exit *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exit) && exit.ExitCode() == 1:
			return &mediaError{check: "signature scan", reason: "flagged it: " + firstLineOf(out.String(), "a match")}
		default:
			return &mediaError{check: "signature scan", reason: "could not run: " + firstLineOf(out.String(), err.Error())}
		}
	}
	a.Checks = append(a.Checks, "re-encoded; signature-scanned clean")
	return nil
}
