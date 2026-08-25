// Copyright (c) the go-macos/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package screencapture is a pure-Go, CGO-free wrapper over Apple's
// ScreenCaptureKit. It enumerates the displays and windows a process may
// capture, and streams a display (or a single window) as raw BGRA pixels.
//
// ScreenCaptureKit is the ONLY capture route left on current macOS: the legacy
// CGDisplayStream path is deprecated and, on macOS 26, no longer produces
// frames. Everything here goes through SCStream.
//
// # The hot path
//
// The package is written for a compositor that redraws every frame and cannot
// afford a copy or an allocation per frame. [Stream.Frame] hands back a
// BORROWED view of the most recent captured frame — the bytes are the
// IOSurface the window server itself rendered into, not a copy — together with
// a boolean saying whether it is newer than the one the previous call
// returned. In steady state a Frame call performs no allocation at all.
//
// The borrow is valid until the next call to [Stream.Frame], [Stream.WaitFrame]
// or [Stream.Close]. Copy out of it (see [Frame.CopyTight] or [Frame.NRGBA])
// if you need to keep it longer.
//
// # Stride
//
// A captured frame's rows are PADDED. Stride is the number of bytes per row
// and it is NOT Width*4 — the window server aligns rows (a 400-pixel-wide
// capture was measured at stride 1664, not 1600). Always index with Stride, or
// use [Frame.Row]. This is the single most common way to get a sheared image.
//
// # Frames only arrive when something changes
//
// ScreenCaptureKit is change-driven. FPS is a CEILING, not a rate: a stream on
// a motionless surface delivers one frame and then nothing until a pixel moves
// (a static wallpaper was measured at 1 frame in 3.1 s). Do not treat a
// missing frame as a failure; treat the "fresh" flag from [Stream.Frame] as
// the truth about whether anything changed.
//
// # Permission
//
// Capturing anything that belongs to another process needs the Screen
// Recording TCC grant. See [Authorized], [RequestAuthorization] and
// [ErrPermissionDenied]. Capturing content owned by the CALLING process
// ([CurrentProcessContent]) needs no grant at all, which is what makes this
// package testable on a machine where the grant is missing.
package screencapture

import (
	"errors"
	"fmt"
	"image"
	"time"
)

// Sentinel errors. All are stable and may be matched with errors.Is.
var (
	// ErrUnsupported is reported on every non-darwin platform, and on a macOS
	// too old to carry ScreenCaptureKit (before 12.3).
	ErrUnsupported = errors.New("screencapture: unsupported on this platform (macOS 12.3 or later only)")

	// ErrPermissionDenied is reported when the Screen Recording TCC grant is
	// missing. Its message names the exact remedy; see also [Authorized].
	ErrPermissionDenied = errors.New("screencapture: Screen Recording permission denied — " +
		"grant it in System Settings > Privacy & Security > Screen & System Audio Recording " +
		"to the application that launched this program (for a program started from a shell " +
		"that is the terminal or editor, not the program itself), then restart that application")

	// ErrNoDisplay is reported when a capture was asked for and the system
	// listed no display at all.
	ErrNoDisplay = errors.New("screencapture: no capturable display")

	// ErrNotFound is reported when a display or window ID does not name
	// anything currently capturable.
	ErrNotFound = errors.New("screencapture: no such display or window")

	// ErrClosed is reported by every [Stream] method after [Stream.Close].
	ErrClosed = errors.New("screencapture: stream is closed")

	// ErrNoFrame is reported by [Stream.WaitFrame] when no frame arrived
	// before its context expired. It is NOT a malfunction: a motionless
	// surface legitimately produces no frames.
	ErrNoFrame = errors.New("screencapture: no frame available")

	// ErrInvalidOption is reported by [Options.Validate] and wraps a
	// description of the offending field.
	ErrInvalidOption = errors.New("screencapture: invalid option")

	// ErrShortBuffer is reported by [Frame.CopyTight] when the destination is
	// too small to hold the frame.
	ErrShortBuffer = errors.New("screencapture: destination buffer too short")
)

// PixelFormat is a CoreVideo OSType naming the layout of a captured frame.
type PixelFormat uint32

// FormatBGRA is 32-bit BGRA, kCVPixelFormatType_32BGRA. It is the only format
// this package streams: it is what a compositor wants, it is what the window
// server produces natively, and it is packed rather than planar so a frame is
// one contiguous run of bytes.
const FormatBGRA PixelFormat = 0x42475241 // 'BGRA'

// String renders the OSType as its four-character code, e.g. "BGRA".
func (f PixelFormat) String() string {
	b := [4]byte{byte(f >> 24), byte(f >> 16), byte(f >> 8), byte(f)}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return fmt.Sprintf("PixelFormat(%#08x)", uint32(f))
		}
	}
	return string(b[:])
}

// BytesPerPixel is the size of one pixel in this format.
func (f PixelFormat) BytesPerPixel() int {
	if f == FormatBGRA {
		return 4
	}
	return 0
}

// Rect is a rectangle in the global desktop coordinate space, in POINTS (not
// pixels). It mirrors CGRect.
type Rect struct {
	X, Y, W, H float64
}

// String renders the rectangle as "(x,y)+(w×h)".
func (r Rect) String() string { return fmt.Sprintf("(%g,%g)+(%g×%g)", r.X, r.Y, r.W, r.H) }

// Empty reports whether the rectangle encloses no area.
func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// Display is a capturable display.
//
// Width and Height are in POINTS, as ScreenCaptureKit reports them. PixelWidth
// and PixelHeight are the display's native backing store in PIXELS, read from
// CoreGraphics — on a Retina display they are the larger pair, and they are
// what you want to hand to [Options] for a capture with no resampling.
type Display struct {
	ID          uint32 // CGDirectDisplayID
	Width       int    // points
	Height      int    // points
	PixelWidth  int    // native pixels
	PixelHeight int    // native pixels
	Frame       Rect   // global desktop position, points
	Main        bool   // this is the display carrying the menu bar
}

// Scale is the display's backing scale factor (pixels per point), 1 when the
// display reports no usable size.
func (d Display) Scale() float64 {
	if d.Width <= 0 {
		return 1
	}
	return float64(d.PixelWidth) / float64(d.Width)
}

// String renders the display for logs.
func (d Display) String() string {
	return fmt.Sprintf("display %d %dx%d pt / %dx%d px at %s", d.ID,
		d.Width, d.Height, d.PixelWidth, d.PixelHeight, d.Frame)
}

// Window is a capturable window.
type Window struct {
	ID       uint32 // CGWindowID
	Title    string
	AppName  string
	BundleID string
	PID      int32
	Frame    Rect // global desktop position, points
	Layer    int  // CoreGraphics window layer; 0 is the normal application layer
	OnScreen bool
	Active   bool
}

// String renders the window for logs.
func (w Window) String() string {
	return fmt.Sprintf("window %d %q [%s] at %s", w.ID, w.Title, w.AppName, w.Frame)
}

// Application is a process owning capturable windows.
type Application struct {
	PID      int32
	Name     string
	BundleID string
}

// Content is a snapshot of what the calling process may capture. It is a
// snapshot: windows open and close, so re-read it rather than caching it.
type Content struct {
	Displays     []Display
	Windows      []Window
	Applications []Application
}

// Display returns the display with the given CGDirectDisplayID.
func (c *Content) Display(id uint32) (Display, error) {
	for _, d := range c.Displays {
		if d.ID == id {
			return d, nil
		}
	}
	return Display{}, fmt.Errorf("%w: display %d", ErrNotFound, id)
}

// MainDisplay returns the display carrying the menu bar, or the first one if
// none is flagged as main.
func (c *Content) MainDisplay() (Display, error) {
	if len(c.Displays) == 0 {
		return Display{}, ErrNoDisplay
	}
	for _, d := range c.Displays {
		if d.Main {
			return d, nil
		}
	}
	return c.Displays[0], nil
}

// Window returns the window with the given CGWindowID.
func (c *Content) Window(id uint32) (Window, error) {
	for _, w := range c.Windows {
		if w.ID == id {
			return w, nil
		}
	}
	return Window{}, fmt.Errorf("%w: window %d", ErrNotFound, id)
}

// WindowsByTitle returns every window whose title is exactly title.
func (c *Content) WindowsByTitle(title string) []Window {
	var out []Window
	for _, w := range c.Windows {
		if w.Title == title {
			out = append(out, w)
		}
	}
	return out
}

// WindowsOfPID returns every window owned by the given process.
func (c *Content) WindowsOfPID(pid int32) []Window {
	var out []Window
	for _, w := range c.Windows {
		if w.PID == pid {
			out = append(out, w)
		}
	}
	return out
}

// Options configures a capture stream.
//
// The zero Options is usable: it captures the source at its native pixel size,
// at up to 60 frames per second, without the cursor.
type Options struct {
	// Width and Height are the requested frame size in PIXELS. Zero means
	// "the source's native pixel size", which for a display is its backing
	// store and for a window is its frame scaled by the display's scale.
	Width, Height int

	// FPS is the CEILING on the frame rate, not a guarantee: ScreenCaptureKit
	// only emits a frame when the content changed. Zero means
	// [DefaultFPS]. It is converted to SCStreamConfiguration's
	// minimumFrameInterval.
	FPS float64

	// ShowsCursor draws the mouse pointer into the captured frames.
	ShowsCursor bool

	// QueueDepth is how many frames ScreenCaptureKit keeps in flight. Zero
	// means [DefaultQueueDepth]. It must leave room for the two frames this
	// package holds on the consumer's behalf (the one lent out and the one
	// waiting), so values below 3 are rejected.
	QueueDepth int

	// ExcludeWindows lists CGWindowIDs to keep out of a DISPLAY capture — for
	// example your own overlay, so capturing the screen it sits on does not
	// feed it back into itself. Ignored for a window capture.
	ExcludeWindows []uint32

	// ScalesToFit letterboxes the source into Width×Height instead of
	// cropping it when the aspect ratios differ.
	ScalesToFit bool
}

// Defaults applied to the zero value of the corresponding [Options] field.
const (
	// DefaultFPS is the frame-rate ceiling used when Options.FPS is zero.
	DefaultFPS = 60.0
	// DefaultQueueDepth is the in-flight frame count used when
	// Options.QueueDepth is zero. Three is the documented minimum that keeps a
	// consumer holding one frame from starving the stream.
	DefaultQueueDepth = 6
	// MinQueueDepth is the smallest queue depth this package accepts.
	MinQueueDepth = 3
	// MaxDimension is the largest frame edge accepted, a sanity bound well
	// above any real display; it exists so a mistaken value fails loudly
	// instead of asking the window server for a terabyte.
	MaxDimension = 32768
)

// Validate reports whether the options are self-consistent, wrapping
// [ErrInvalidOption]. It does not consult the system.
func (o Options) Validate() error {
	if o.Width < 0 || o.Height < 0 {
		return fmt.Errorf("%w: negative size %dx%d", ErrInvalidOption, o.Width, o.Height)
	}
	if (o.Width == 0) != (o.Height == 0) {
		return fmt.Errorf("%w: Width and Height must both be set or both be zero, got %dx%d",
			ErrInvalidOption, o.Width, o.Height)
	}
	if o.Width > MaxDimension || o.Height > MaxDimension {
		return fmt.Errorf("%w: size %dx%d exceeds the %d-pixel limit",
			ErrInvalidOption, o.Width, o.Height, MaxDimension)
	}
	if o.FPS < 0 {
		return fmt.Errorf("%w: negative FPS %g", ErrInvalidOption, o.FPS)
	}
	if o.FPS > 0 && o.FPS < 0.01 {
		return fmt.Errorf("%w: FPS %g is below the 0.01 minimum", ErrInvalidOption, o.FPS)
	}
	if o.QueueDepth < 0 {
		return fmt.Errorf("%w: negative QueueDepth %d", ErrInvalidOption, o.QueueDepth)
	}
	if o.QueueDepth > 0 && o.QueueDepth < MinQueueDepth {
		return fmt.Errorf("%w: QueueDepth %d is below the minimum of %d",
			ErrInvalidOption, o.QueueDepth, MinQueueDepth)
	}
	return nil
}

// resolve fills the zero fields from the defaults and from the source's native
// pixel size, and returns the options actually used. It validates first, so a
// resolved Options is always usable.
func (o Options) resolve(nativeW, nativeH int) (Options, error) {
	if err := o.Validate(); err != nil {
		return Options{}, err
	}
	r := o
	if r.Width == 0 {
		if nativeW <= 0 || nativeH <= 0 {
			return Options{}, fmt.Errorf("%w: no size given and the source reports %dx%d",
				ErrInvalidOption, nativeW, nativeH)
		}
		r.Width, r.Height = nativeW, nativeH
	}
	if r.FPS == 0 {
		r.FPS = DefaultFPS
	}
	if r.QueueDepth == 0 {
		r.QueueDepth = DefaultQueueDepth
	}
	return r, nil
}

// frameIntervalTimescale is the timescale every minimumFrameInterval CMTime is
// stated in. A microsecond timescale states 1/60 s, 1/59.94 s and 1/120 s all
// within a microsecond, and never overflows the int64 numerator.
const frameIntervalTimescale = int32(1_000_000)

// frameInterval converts a frame-rate ceiling to the CMTime numerator used for
// SCStreamConfiguration.minimumFrameInterval. A non-positive fps yields the
// interval for [DefaultFPS] rather than a division by zero.
func frameInterval(fps float64) (value int64, timescale int32) {
	if fps <= 0 {
		fps = DefaultFPS
	}
	v := int64(float64(frameIntervalTimescale)/fps + 0.5)
	if v < 1 {
		v = 1 // a rate faster than the timescale clamps to one tick
	}
	return v, frameIntervalTimescale
}

// Frame is a BORROWED view of one captured frame.
//
// Pix aliases memory owned by the window server. It stays valid only until the
// next [Stream.Frame], [Stream.WaitFrame] or [Stream.Close] on the stream that
// produced it. Do not retain it; copy with [Frame.CopyTight] or [Frame.NRGBA]
// if you need it to outlive the borrow.
type Frame struct {
	// Pix is the frame's bytes in [FormatBGRA], Stride bytes per row,
	// Height rows. len(Pix) == Stride*Height.
	Pix []byte
	// Width and Height are the frame's size in pixels.
	Width, Height int
	// Stride is the number of BYTES per row. It is padded and is NOT
	// necessarily Width*4.
	Stride int
	// Seq counts frames since the stream started; it is 0 before the first
	// frame and strictly increases afterwards.
	Seq uint64
	// At is when the delivery callback received the frame.
	At time.Time
}

// Valid reports whether the frame holds pixels.
func (f Frame) Valid() bool {
	return f.Width > 0 && f.Height > 0 && f.Stride >= f.Width*4 && len(f.Pix) >= f.Stride*f.Height
}

// TightLen is the number of bytes the frame occupies with no row padding,
// Width*4*Height.
func (f Frame) TightLen() int { return f.Width * 4 * f.Height }

// Row returns row y of the frame, Width*4 bytes with the padding trimmed off.
// It does not allocate. It returns nil for an out-of-range y or an invalid
// frame.
func (f Frame) Row(y int) []byte {
	if !f.Valid() || y < 0 || y >= f.Height {
		return nil
	}
	off := y * f.Stride
	return f.Pix[off : off+f.Width*4 : off+f.Width*4]
}

// CopyTight copies the frame into dst with the row padding removed, so dst
// holds Width*4*Height bytes of contiguous BGRA. It reports how many bytes it
// wrote, or [ErrShortBuffer] if dst is too small. It allocates nothing.
func (f Frame) CopyTight(dst []byte) (int, error) {
	if !f.Valid() {
		return 0, ErrNoFrame
	}
	n := f.TightLen()
	if len(dst) < n {
		return 0, fmt.Errorf("%w: need %d bytes, got %d", ErrShortBuffer, n, len(dst))
	}
	rowLen := f.Width * 4
	// The fast path: an unpadded frame is one contiguous run. It happens
	// whenever the width lands on the window server's row alignment, which for
	// a full-width display capture it usually does.
	if f.Stride == rowLen {
		return copy(dst, f.Pix[:n]), nil
	}
	for y := 0; y < f.Height; y++ {
		src := y * f.Stride
		copy(dst[y*rowLen:(y+1)*rowLen], f.Pix[src:src+rowLen])
	}
	return n, nil
}

// NRGBA copies the frame into a freshly allocated image.NRGBA, swapping BGRA
// to RGBA as it goes. It is the convenience path for saving a frame to disk;
// it allocates, so it does not belong in a per-frame loop.
func (f Frame) NRGBA() (*image.NRGBA, error) {
	if !f.Valid() {
		return nil, ErrNoFrame
	}
	img := image.NewNRGBA(image.Rect(0, 0, f.Width, f.Height))
	for y := 0; y < f.Height; y++ {
		src := f.Pix[y*f.Stride:]
		dst := img.Pix[y*img.Stride:]
		for x := 0; x < f.Width; x++ {
			// BGRA -> RGBA. ScreenCaptureKit hands back opaque frames, but
			// the alpha byte is carried through rather than forced to 255 so
			// a window capture with a transparent corner stays honest.
			dst[x*4+0] = src[x*4+2]
			dst[x*4+1] = src[x*4+1]
			dst[x*4+2] = src[x*4+0]
			dst[x*4+3] = src[x*4+3]
		}
	}
	return img, nil
}

// Stats reports what a stream has seen since it started.
type Stats struct {
	// Frames is the number of frames actually delivered with pixels.
	Frames uint64
	// Idle is the number of callbacks that carried no image, which is how
	// ScreenCaptureKit says "nothing changed".
	Idle uint64
	// Superseded is the number of delivered frames that were replaced by a
	// newer one before the consumer ever asked for them. A large value next to
	// Frames means the consumer is slower than the capture.
	Superseded uint64
	// Last is when the most recent frame with pixels arrived.
	Last time.Time
	// Interval is the gap between the two most recent frames with pixels.
	Interval time.Duration
}

// FPS is the instantaneous rate implied by [Stats.Interval], 0 when fewer than
// two frames have arrived.
func (s Stats) FPS() float64 {
	if s.Interval <= 0 {
		return 0
	}
	return float64(time.Second) / float64(s.Interval)
}

// StreamError is an error reported by ScreenCaptureKit itself, carrying the
// SCStreamErrorDomain code. Codes this package recognises unwrap to a sentinel
// — notably -3801 (SCStreamErrorUserDeclined) unwraps to [ErrPermissionDenied]
// — so errors.Is works without anyone having to know the numbers.
type StreamError struct {
	// Code is the NSError code in SCStreamErrorDomain.
	Code int
	// Name is Apple's constant for Code, or "" for a code this package does
	// not know.
	Name string
	// Message is the NSError's localizedDescription.
	Message string
	// Op names the operation that failed, e.g. "getShareableContent".
	Op string
}

// Error renders the code, Apple's name for it and the system's message.
func (e *StreamError) Error() string {
	name := e.Name
	if name == "" {
		name = "SCStreamError"
	}
	op := e.Op
	if op == "" {
		op = "ScreenCaptureKit"
	}
	msg := e.Message
	if msg == "" {
		msg = "no message"
	}
	if e.Code == errUserDeclined {
		// The system's own wording for -3801 ("the user declined TCCs") tells
		// nobody what to actually do, so the remedy is spliced in here.
		return fmt.Sprintf("screencapture: %s: %s (%d): %s; %s",
			op, name, e.Code, msg, ErrPermissionDenied)
	}
	return fmt.Sprintf("screencapture: %s: %s (%d): %s", op, name, e.Code, msg)
}

// Unwrap maps the codes with a sentinel to that sentinel.
func (e *StreamError) Unwrap() error {
	switch e.Code {
	case errUserDeclined, errMissingEntitlements:
		return ErrPermissionDenied
	case errNoDisplayList, errNoCaptureSource:
		return ErrNoDisplay
	case errNoWindowList:
		return ErrNotFound
	}
	return nil
}

// SCStreamErrorDomain codes, from ScreenCaptureKit's SCError.h. Only the ones
// this package can actually surface are named; the rest are covered by
// streamErrorNames.
const (
	errUserDeclined        = -3801
	errMissingEntitlements = -3803
	errNoWindowList        = -3813
	errNoDisplayList       = -3814
	errNoCaptureSource     = -3815
)

// streamErrorNames maps every SCStreamErrorDomain code to Apple's constant, so
// a failure reads as a name rather than as a bare negative number. Taken from
// SCError.h; -3801 is the one observed live on this fleet.
var streamErrorNames = map[int]string{
	-3801: "SCStreamErrorUserDeclined",
	-3802: "SCStreamErrorFailedToStart",
	-3803: "SCStreamErrorMissingEntitlements",
	-3804: "SCStreamErrorFailedApplicationConnectionInvalid",
	-3805: "SCStreamErrorFailedApplicationConnectionInterrupted",
	-3806: "SCStreamErrorFailedNoMatchingApplicationContext",
	-3807: "SCStreamErrorAttemptToStartStreamState",
	-3808: "SCStreamErrorAttemptToStopStreamState",
	-3809: "SCStreamErrorAttemptToUpdateFilterState",
	-3810: "SCStreamErrorAttemptToConfigState",
	-3811: "SCStreamErrorInternalError",
	-3812: "SCStreamErrorInvalidParameter",
	-3813: "SCStreamErrorNoWindowList",
	-3814: "SCStreamErrorNoDisplayList",
	-3815: "SCStreamErrorNoCaptureSource",
	-3816: "SCStreamErrorRemovingStream",
	-3817: "SCStreamErrorUserStopped",
	-3818: "SCStreamErrorFailedToStartAudioCapture",
	-3819: "SCStreamErrorFailedToStopAudioCapture",
	-3820: "SCStreamErrorFailedToStartMicrophoneCapture",
	-3821: "SCStreamErrorSystemStoppedStream",
}

// newStreamError builds a [StreamError] for op from an NSError's code and
// localizedDescription.
func newStreamError(op string, code int, message string) *StreamError {
	return &StreamError{Code: code, Name: streamErrorNames[code], Message: message, Op: op}
}
