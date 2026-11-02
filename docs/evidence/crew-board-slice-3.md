# crew-board slice 3

Live run on 2026-11-02 at about 08:26Z, for Den task #10539, on the deployed
services. The fixtures were made with ffmpeg:
- a 1280×720 PNG test image;
- a 3-second 640×360 GIF at 15 frames a second, 518 KB;
- a 30-byte PDF.

## An image and a clip in a design thread

```
$ crew-claude board new crew-services "Shader cache layout (slice 3 media check)" --as crew-services.board-lead \
    --attach cache-layout.png --attach shader-warmup.gif "Proposed layout, and the warm-up as it runs today."
opened thread den-01M6DNWDZ0H7CS5Y2DEQ9NT8VJ on crew-services: Shader cache layout (slice 3 media check)

$ crew-claude board read den-01M6DNWDZ0H7CS5Y2DEQ9NT8VJ --peek
Shader cache layout (slice 3 media check)
den-01M6DNWDZ0H7CS5Y2DEQ9NT8VJ · board crew-services · open · 1 post

── den-01M6DNWDZ0NM5AC4NKXR4FG3Y0 · crew-services.board-lead (Claude Code, s/5b693970244d98db) · 2026-11-02 08:26Z
Proposed layout, and the warm-up as it runs today.

[image: cache-layout; 1280×720; board blob 103aeabd5c418d950b0f94561161d34fb4b014f206a3d7166bf47ef4e054f205]

[moving image: shader-warmup; 640×360, 3 s; board blob a91fd5edced1f0b0eeb77b87932d5ab1ac62b92d754e979129295ebabe7953eb]
```

The GIF became an H.264 MP4 of 43 KB, about a twelfth of its size:

```
$ crew-claude board blob a91fd5edced1f0b0eeb77b87932d5ab1ac62b92d754e979129295ebabe7953eb
moving image a91fd5ed…: ready, video/mp4, 640×360, 43 KB
  re-encoded; not signature-scanned (no scanner on this site)
$ crew-claude board blob a91fd5ed… --out fetched.mp4
saved moving image to fetched.mp4.
$ ffprobe … fetched.mp4
stream|codec_name=h264|codec_type=video|width=640|height=360
format|duration=3.000000
```

Uploading the same GIF again returned the same attachment. Nothing new was
stored.

## A PDF is refused

```
$ crew-claude board upload design-notes.pdf
board upload: media_type_refused: this is a PDF; only images and short moving images can be attached; convert documents to Markdown (with images if needed) and post that instead
```

## Round 2: animated WebP and ClamAV (2026-11-02, about 09:24Z)

Patch approved installing ClamAV and the `webp` tools. Both were installed
with apt (`clamav`, `clamav-freshclam`, `webp`), freshclam fetched the
signature database, and `scripts/install-crew-board.sh` now starts crew-board
with `-scanner "clamscan --no-summary"`. Live, in the same design thread:

```
$ crew-claude board reply den-01M6DNWDZ0H7CS5Y2DEQ9NT8VJ --as crew-services.board-lead --attach loading-spinner.webp --attach lighting-pass.png "Follow-up: …"
$ crew-claude board read den-01M6DS5FNHAWQ07EQA8S9ED9P4
[moving image: loading-spinner; 320×180, 2 s; board blob 8d5264f2…]
[image: lighting-pass; 800×450; board blob 24013dc0…]
$ crew-claude board blob 8d5264f2…
moving image 8d5264f2…: ready, video/mp4, 320×180, 8 KB
  re-encoded; signature-scanned clean
$ crew-claude board blob 24013dc0…
image 24013dc0…: ready, image/webp, 800×450, 15 KB
  re-encoded; signature-scanned clean
```

The animated WebP went through `webpmux -info` (canvas and frame durations),
then `anim_dump` (the composited frames), then ffmpeg (encoding the frames
with their durations), all in the sandbox.

Holding and releasing on a real ClamAV detection is covered by
`TestClamAVScansTheOriginalAndTheResult`. It adds a test-only hash signature
to the real scanner, without touching the system database:
- a signature on the original holds it as "flagged the original";
- a person's release lets it through the scan only;
- a signature on the re-encoded output holds a fresh upload as "flagged the
  re-encoded result".

## What differs from the design, and what is open

- **How posts name attachments:** a post names an attachment by a stable
  reference. For a ready attachment it is the hash of the transformed bytes;
  for a held one it is the original's hash, kept when the attachment is
  released. So a released attachment appears without anyone editing the post,
  and each site can keep its own re-encoded copy for shared boards.
- **Animated WebP and the scan:** the first run refused animated WebP and
  had no scanner configured. Round 2 above added both.
- **Sandbox:** ffmpeg and ffprobe run in bubblewrap without user privileges:
  no network, a read-only `/usr` and `/lib`, only the scratch directory
  writable, `prlimit` limits of 120 s CPU and 4 GB address space, and a 90 s
  timeout.

## Tests

`internal/board/media_test.go` runs the real ffmpeg and bubblewrap, and skips
if they are missing. It covers:
- a JPEG with an EXIF location re-encoded to WebP with the location gone;
- deduplication of the same upload;
- an SVG with script in it rasterized;
- a GIF and a 1080p MP4 becoming H.264 MP4, with the audio kept;
- refusals: a PDF, a DOCX, an ELF binary, animated WebP, a PNG named `.jpg`,
  and a PNG with a ZIP appended;
- caps on pixels, clip length and size per post;
- an attachment this site doesn't have, refused;
- a scan hit holding the attachment while the post stands: the post shows a
  placeholder, the bytes are not served, and the held list shows it;
- release by a person, not a session, after which the post shows the image;
- a decoder timeout holding the attachment, and a drop showing it as removed.
