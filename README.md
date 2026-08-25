# go-macos/screencapture

Screen and window capture on macOS from pure Go — `CGO_ENABLED=0`, via
[purego](https://github.com/ebitengine/purego) and
[go-macos/objc](https://github.com/go-macos/objc). A thin, honest wrapper over
**ScreenCaptureKit**, built for a compositor that redraws every frame.

```go
content, _ := screencapture.Shareable(ctx)
display, _ := content.MainDisplay()

s, err := screencapture.CaptureDisplay(ctx, display, screencapture.Options{
        FPS: 60, // a CEILING, not a promise
})
if err != nil {
        return err // errors.Is(err, screencapture.ErrPermissionDenied) says exactly what to do
}
defer s.Close()

for {
        f, fresh := s.Frame() // BORROWED bytes, no copy, no allocation
        if fresh {
                composite(f.Pix, f.Width, f.Height, f.Stride) // BGRA, PADDED rows
        }
        render()
}
```

## Why it looks like this

The consumer is an XR virtual-desktop app that composites several captured
screens into a panorama every frame, on a 16.6 ms budget. Two decisions follow
from that and shape the whole API.

**`Frame()` lends, it does not copy.** `Frame.Pix` aliases the IOSurface the
window server itself rendered into. The package retains and locks the
`CVPixelBuffer` behind it and hands you the bytes; the borrow lasts until your
next `Frame`, `WaitFrame` or `Close`. Measured on an M4 Max: **25.9 ns and zero
allocations per call**, at any frame size.

**Stride is in the API, not assumed.** Captured rows are padded. A 400-pixel-wide
capture came back with a stride of **1664 bytes, not 1600** — 64 bytes of padding
per row. Index with `f.Stride`, or use `f.Row(y)`. This is the single most common
way to get a sheared image out of ScreenCaptureKit, so the type makes it hard to
get wrong.

## Frames only arrive when something changes

ScreenCaptureKit is change-driven, and this surprises everyone once.
`Options.FPS` is a **ceiling**. A stream on a motionless surface delivers one
frame and then nothing: a static wallpaper was measured at **1 frame in 3.1
seconds**. That is not a failure — it is the API telling you nothing moved.
`Frame()`'s second return value, not a timer, is the truth about whether there is
new content.

## Permission

Capturing anything owned by another process needs the **Screen Recording** TCC
grant.

```go
if !screencapture.Authorized() {          // CGPreflightScreenCaptureAccess: no prompt
        screencapture.RequestAuthorization() // CGRequestScreenCaptureAccess: may prompt
}
```

A denial is a named error, never a bare nil, and its message is the remedy:

```
screencapture: getShareableContent: SCStreamErrorUserDeclined (-3801):
The user declined TCCs for application, window, display capture;
screencapture: Screen Recording permission denied — grant it in System Settings >
Privacy & Security > Screen & System Audio Recording to the application that
launched this program (for a program started from a shell that is the terminal or
editor, not the program itself), then restart that application
```

`errors.Is(err, screencapture.ErrPermissionDenied)` matches it.

Two things worth knowing. The grant belongs to the **responsible application** —
for a binary started from a shell that is the terminal or the editor, not your
binary. And once an application has been refused, macOS records the refusal and
shows **no further prompt**: `RequestAuthorization` then returns `false`
immediately, and only System Settings can change the answer.

### Your own windows need no permission at all

Content owned by the calling process is capturable with no grant whatsoever.

```go
content, _ := screencapture.CurrentProcessShareable(ctx) // never needs permission
win, _ := content.Window(myWindowID)
s, _ := screencapture.CaptureWindow(ctx, win, screencapture.Options{})
```

`CaptureWindow` tries the permissioned enumeration first and falls back to this
one, so capturing your own window works on a machine that never granted Screen
Recording. It is also how this package's live tests prove themselves on such a
machine.

## API

```go
func Available() bool               // ScreenCaptureKit is present
func Authorized() bool              // may I capture right now? (no prompt)
func RequestAuthorization() bool    // ask the system (may prompt, once ever)

func Shareable(ctx) (*Content, error)               // needs the grant
func CurrentProcessShareable(ctx) (*Content, error) // needs nothing
func Displays(ctx) ([]Display, error)
func Windows(ctx) ([]Window, error)

func CaptureDisplay(ctx, Display, Options) (*Stream, error)
func CaptureWindow(ctx, Window, Options) (*Stream, error)

func (s *Stream) Frame() (Frame, bool)              // borrowed; bool = fresh
func (s *Stream) WaitFrame(ctx) (Frame, error)      // blocks for a NEW frame
func (s *Stream) Stats() Stats
func (s *Stream) Options() Options
func (s *Stream) Err() error                        // why the SYSTEM stopped us
func (s *Stream) Close() error                      // idempotent

type Frame struct {
        Pix           []byte    // BGRA, Stride bytes per row — BORROWED
        Width, Height int
        Stride        int       // NOT Width*4
        Seq           uint64
        At            time.Time
}

func (f Frame) Row(y int) []byte              // one row, padding trimmed, no alloc
func (f Frame) CopyTight(dst []byte) (int, error) // depad into your buffer, no alloc
func (f Frame) NRGBA() (*image.NRGBA, error)  // allocates; for saving to disk
```

`Options`'s zero value captures the source at its native pixel size, at up to 60
fps, without the cursor.

## Measured

On an Apple M4 Max, macOS 26.6.2, Go 1.26.4, `CGO_ENABLED=0`.

| | |
|---|---|
| `Stream.Frame()` | **25.9 ns/op, 0 B/op, 0 allocs/op** |
| 1280×720 capture | 103.6 fps sustained, 0 allocations per frame |
| 3840×1080 capture ("panorama") | 107.9 fps, 15.8 MiB per frame, 0 allocations per frame |
| `CopyTight`, 1280×720 | 0.058 ms (only if you cannot use a padded buffer) |
| `CopyTight`, 3840×1080 | 0.36 ms, 46 GB/s |

The capture path costs the consumer essentially nothing: the window server has
already rendered the pixels, and `Frame()` hands over a pointer.

## Verifying it yourself

```sh
# What can this machine see? Does it have the permission?
go run ./cmd/sccheck
go run ./cmd/sccheck -windows
go run ./cmd/sccheck -request

# Capture 30 frames of the main display and save the last one.
go run ./cmd/sccheck -n 30 -o /tmp/display.png

# The unit suite: no display, no permission, runs anywhere.
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 GOOS=linux go test ./...

# The LIVE proof. It opens a real NSWindow of its own, captures it, and asserts
# that the centre pixel is the colour the window was just painted — and that it
# CHANGES. It needs no permission, because the window belongs to this process.
SCREENCAPTURE_INTEGRATION=1 go test -tags integration -v -run TestLive .
SCREENCAPTURE_INTEGRATION=1 go test -tags integration -run '^$' \
        -bench . -benchmem -benchtime 500000x .
```

The live suite writes a PNG to `testdata/artifacts/window-capture.png` so a human
can look at what was actually captured.

## What this package does not do

- **No audio.** `SCStreamConfiguration` can capture system audio and the
  microphone; this package asks for screen output only.
- **No pixel format but BGRA.** It is what a compositor wants and what the window
  server produces natively.
- **No `SCScreenshotManager`.** One-shot screenshots are a different shape of
  problem; open a stream and take one frame.
- **No `SCContentSharingPicker`.** The system picker is an AppKit UI flow.

## Platforms

macOS 12.3 or later (13+ in practice; `CurrentProcessShareable` needs 14). Every
other platform compiles and reports `ErrUnsupported`, so consumers cross-compile
without a build tag.

`CGDisplayStream`, the legacy path, is deprecated and no longer produces frames
on current macOS. ScreenCaptureKit is the only route.

## Licence

BSD-3-Clause.
