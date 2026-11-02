package board_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crew-services/internal/board"
)

// fixture makes a media file with ffmpeg (outside any sandbox).
func fixture(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("ffmpeg", append(append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...), path)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make %s: %v\n%s", name, err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mediaWorld(t *testing.T, tune func(*board.MediaConfig)) (*world, *board.Ingester) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe", "bwrap", "prlimit"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	w := newWorld(t)
	cfg := board.DefaultMediaConfig(filepath.Join(w.dir, "media"))
	if tune != nil {
		tune(&cfg)
	}
	in, err := board.NewIngester(cfg, func() time.Time { return w.now })
	if err != nil {
		t.Fatal(err)
	}
	w.svc.UseMedia(in, w.store)
	w.newBoard("rusty-engine")
	return w, in
}

// withExif puts a fake EXIF segment carrying a location into a JPEG.
func withExif(jpeg []byte) []byte {
	payload := []byte("Exif\x00\x00GPS 37.8N 122.4W SECRETPLACE")
	segment := append([]byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}, payload...)
	return append(append(append([]byte{}, jpeg[:2]...), segment...), jpeg[2:]...)
}

func TestImagesAreReencodedWithoutMetadata(t *testing.T) {
	w, in := mediaWorld(t, nil)
	jpeg := withExif(fixture(t, "photo.jpg", "-f", "lavfi", "-i", "testsrc=size=320x240", "-frames:v", "1"))
	if !bytes.Contains(jpeg, []byte("SECRETPLACE")) {
		t.Fatal("fixture lacks its EXIF")
	}
	a, err := w.svc.Upload(w.ctx, "photo.jpg", jpeg)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != board.AttachmentReady || a.Kind != board.MediaImage || a.Type != "image/webp" || a.Width != 320 || a.Height != 240 || a.Ref != a.OutputHash {
		t.Fatalf("attachment = %+v", a)
	}
	stored, err := os.ReadFile(in.BlobPath(a))
	if err != nil || !bytes.HasPrefix(stored, []byte("RIFF")) || bytes.Contains(stored, []byte("SECRETPLACE")) {
		t.Fatalf("stored blob: %v, starts %q, has EXIF %v", err, stored[:min(12, len(stored))], bytes.Contains(stored, []byte("SECRETPLACE")))
	}
	if !strings.Contains(strings.Join(a.Checks, ";"), "not signature-scanned") {
		t.Fatalf("checks = %v", a.Checks)
	}
	// The same bytes again are the same attachment.
	again, err := w.svc.Upload(w.ctx, "copy.jpg", jpeg)
	if err != nil || again.Ref != a.Ref {
		t.Fatalf("second upload = %+v, %v", again, err)
	}
	// SVG, even with script in it, is rasterized.
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="40" height="30"><script>alert(1)</script><rect width="40" height="30" fill="blue"/></svg>`)
	s, err := w.svc.Upload(w.ctx, "diagram.svg", svg)
	if err != nil || s.Type != "image/webp" || s.Width != 40 {
		t.Fatalf("svg = %+v, %v", s, err)
	}
	if out, _ := os.ReadFile(in.BlobPath(s)); bytes.Contains(out, []byte("script")) {
		t.Fatal("svg script survived")
	}
}

func TestMovingImagesBecomeOneKindAndKeepAudio(t *testing.T) {
	w, in := mediaWorld(t, nil)
	gif := fixture(t, "spin.gif", "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10", "-t", "1")
	g, err := w.svc.Upload(w.ctx, "spin.gif", gif)
	if err != nil || g.Kind != board.MediaMotion || g.Type != "video/mp4" || g.DurationMS < 500 || g.Width != 160 {
		t.Fatalf("gif = %+v, %v", g, err)
	}
	clip := fixture(t, "clip.mp4", "-f", "lavfi", "-i", "testsrc=size=1920x1080:rate=24", "-f", "lavfi", "-i", "sine=frequency=440", "-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest")
	c, err := w.svc.Upload(w.ctx, "clip.mp4", clip)
	if err != nil || c.Kind != board.MediaMotion || c.Width != 1920 || c.Height != 1080 {
		t.Fatalf("clip = %+v, %v", c, err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", in.BlobPath(c)).Output()
	if err != nil || !strings.Contains(string(out), "audio") {
		t.Fatalf("clip streams = %q, %v", out, err)
	}
}

func TestWhatIsNotMediaIsRefused(t *testing.T) {
	w, _ := mediaWorld(t, nil)
	png := fixture(t, "x.png", "-f", "lavfi", "-i", "color=red:size=16x16", "-frames:v", "1")
	animated := fixture(t, "x.webp", "-f", "lavfi", "-i", "testsrc=size=32x32:rate=5", "-t", "1", "-c:v", "libwebp_anim", "-loop", "0")
	cases := []struct {
		name string
		data []byte
		want board.ErrorCode
		text string
	}{
		{"notes.pdf", []byte("%PDF-1.7\n1 0 obj\n"), board.CodeMediaTypeRefused, "convert documents to Markdown"},
		{"plan.docx", append([]byte("PK\x03\x04"), []byte("....[Content_Types].xml....word/document.xml")...), board.CodeMediaTypeRefused, "Office document"},
		{"tool", []byte("\x7fELF\x02\x01\x01"), board.CodeMediaTypeRefused, "a program"},
		{"spin.webp", animated, board.CodeMediaTypeRefused, "animated WebP"},
		{"x.jpg", png, board.CodeMediaTypeMismatch, "the name says JPEG but the bytes are PNG"},
		{"x.png", append(append([]byte{}, png...), []byte("PK\x05\x06\x00\x00")...), board.CodeMediaTypeRefused, "ZIP archive"},
	}
	for _, c := range cases {
		_, err := w.svc.Upload(w.ctx, c.name, c.data)
		if board.CodeOf(err) != c.want || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: %v, want %s mentioning %q", c.name, err, c.want, c.text)
		}
	}
}

func TestCapsAreEnforced(t *testing.T) {
	w, _ := mediaWorld(t, func(c *board.MediaConfig) {
		c.StillPixels, c.MotionSeconds, c.PostBytes = 50*50, 1, 100
	})
	big := fixture(t, "big.png", "-f", "lavfi", "-i", "testsrc=size=100x100", "-frames:v", "1")
	if _, err := w.svc.Upload(w.ctx, "big.png", big); board.CodeOf(err) != board.CodeMediaTooLarge || !strings.Contains(err.Error(), "100×100") {
		t.Fatalf("over the pixel cap: %v", err)
	}
	long := fixture(t, "long.gif", "-f", "lavfi", "-i", "testsrc=size=32x32:rate=5", "-t", "3")
	if _, err := w.svc.Upload(w.ctx, "long.gif", long); board.CodeOf(err) != board.CodeMediaTooLarge || !strings.Contains(err.Error(), "at most 1 s") {
		t.Fatalf("over the duration cap: %v", err)
	}
	var refs []string
	for _, color := range []string{"red", "green", "blue"} {
		a, err := w.svc.Upload(w.ctx, color+".png", fixture(t, color+".png", "-f", "lavfi", "-i", "testsrc2=size=48x48", "-vf", "drawbox=color="+color+":t=fill:w=8:h=8", "-frames:v", "1"))
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, "![x](blob:sha256-"+a.Ref+")")
	}
	_, _, err := w.svc.OpenThread(w.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: w.op(), Board: "rusty-engine", Title: "Too much", Body: strings.Join(refs, "\n")})
	if board.CodeOf(err) != board.CodeMediaTooLarge {
		t.Fatalf("over the per-post cap: %v", err)
	}
	_, _, err = w.svc.OpenThread(w.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: w.op(), Board: "rusty-engine", Title: "Unknown", Body: "![x](blob:sha256-" + strings.Repeat("ab", 32) + ")"})
	if board.CodeOf(err) != board.CodeAttachmentUnknown {
		t.Fatalf("an attachment this site lacks: %v", err)
	}
}

func TestAFailedCheckHoldsTheAttachmentNotThePost(t *testing.T) {
	w, _ := mediaWorld(t, func(c *board.MediaConfig) {
		c.Scanner = []string{"sh", "-c", "exit 1", "--"}
	})
	png := fixture(t, "x.png", "-f", "lavfi", "-i", "color=green:size=24x24", "-frames:v", "1")
	held, err := w.svc.Upload(w.ctx, "x.png", png)
	if err != nil || held.State != board.AttachmentHeld || !strings.HasPrefix(held.Reason, "signature scan: flagged it") {
		t.Fatalf("scan hit = %+v, %v", held, err)
	}
	opened := w.thread(board.Actor{Person: "patch"}, "With a held image", "Look:\n\n![logo](blob:sha256-"+held.Ref+")")
	post, _ := w.svc.GetPost(w.ctx, opened.Post.ID)
	if text := board.RenderForAgentWith(post.Body, false, post.Attachments); !strings.Contains(text, "[attachment held: signature scan: flagged it") || !strings.Contains(text, "board release "+held.Ref) {
		t.Fatalf("held render = %q", text)
	}
	if _, _, err := w.svc.BlobPath(w.ctx, held.Ref); board.CodeOf(err) != board.CodeAttachmentHeld {
		t.Fatalf("fetching held bytes = %v", err)
	}
	listed, _ := w.svc.Held(w.ctx)
	if len(listed) != 1 || listed[0].Ref != held.Ref {
		t.Fatalf("held list = %+v", listed)
	}
	if _, err := w.svc.Release(w.ctx, board.HeldRequest{Actor: board.Actor{SessionAddress: w.a}, Ref: held.Ref}); board.CodeOf(err) != board.CodePersonOnly {
		t.Fatalf("a session releasing = %v", err)
	}
	released, err := w.svc.Release(w.ctx, board.HeldRequest{Actor: board.Actor{Person: "lore"}, Ref: held.Ref})
	if err != nil || released.State != board.AttachmentReady || released.Ref != held.Ref || !strings.Contains(strings.Join(released.Checks, ";"), "released by lore") {
		t.Fatalf("released = %+v, %v", released, err)
	}
	post, _ = w.svc.GetPost(w.ctx, opened.Post.ID)
	if text := board.RenderForAgentWith(post.Body, false, post.Attachments); !strings.Contains(text, "[image: logo; 24×24; board blob "+held.Ref+"]") {
		t.Fatalf("render after release = %q", text)
	}
	if _, path, err := w.svc.BlobPath(w.ctx, held.Ref); err != nil || !strings.HasSuffix(path, ".webp") {
		t.Fatalf("released bytes = %s, %v", path, err)
	}

	// A decoder that runs out of time holds the attachment too, and dropping it
	// leaves the post saying so.
	w2, _ := mediaWorld(t, func(c *board.MediaConfig) { c.Timeout = 1 })
	slow, err := w2.svc.Upload(w2.ctx, "slow.png", png)
	if err != nil || slow.State != board.AttachmentHeld || !strings.Contains(slow.Reason, "decoder: timed out") {
		t.Fatalf("decoder timeout = %+v, %v", slow, err)
	}
	if _, err := w2.svc.Drop(w2.ctx, board.HeldRequest{Actor: board.Actor{Person: "patch"}, Ref: slow.Ref}); err != nil {
		t.Fatal(err)
	}
	gone, _ := w2.svc.Attachment(w2.ctx, slow.Ref)
	if text := board.RenderForAgentWith("![x](blob:sha256-"+slow.Ref+")", false, []board.Attachment{gone}); text != "[attachment removed]" {
		t.Fatalf("dropped render = %q", text)
	}
}
