// Copyright (c) the go-macos/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package screencapture

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/go-macos/objc"
)

// Framework paths. ScreenCaptureKit carries the capture API itself, CoreMedia
// the CMSampleBuffer the frames arrive in, CoreVideo the CVPixelBuffer inside
// it, and CoreGraphics both the TCC preflight and the native pixel size of a
// display (which SCDisplay reports only in points).
const (
	frameworkScreenCaptureKit = "/System/Library/Frameworks/ScreenCaptureKit.framework/ScreenCaptureKit"
	frameworkCoreMedia        = "/System/Library/Frameworks/CoreMedia.framework/CoreMedia"
	frameworkCoreVideo        = "/System/Library/Frameworks/CoreVideo.framework/CoreVideo"
	frameworkCoreGraphics     = "/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics"
)

// SCStreamOutputType. Only the screen type is handled; audio and microphone
// output would arrive on the same callback and are ignored.
const outputTypeScreen = 0

// cvReadOnly is CVPixelBufferLockFlags' kCVPixelBufferLock_ReadOnly. Locking
// read-only lets the window server keep the surface wherever it likes; a
// read-write lock can force a copy out of GPU-visible memory, which is exactly
// what this package exists to avoid.
const cvReadOnly = uint64(1)

// stopTimeout bounds the wait for -stopCaptureWithCompletionHandler:. Close
// must not hang a consumer's shutdown because the window server is wedged.
const stopTimeout = 3 * time.Second

var (
	cmSampleBufferGetImageBuffer func(uintptr) uintptr
	cmSampleBufferIsValid        func(uintptr) bool

	cvPixelBufferLockBaseAddress   func(uintptr, uint64) int32
	cvPixelBufferUnlockBaseAddress func(uintptr, uint64) int32
	cvPixelBufferGetBaseAddress    func(uintptr) unsafe.Pointer
	cvPixelBufferGetBytesPerRow    func(uintptr) uint64
	cvPixelBufferGetWidth          func(uintptr) uint64
	cvPixelBufferGetHeight         func(uintptr) uint64
	cvBufferRetain                 func(uintptr) uintptr
	cvBufferRelease                func(uintptr)

	cgPreflightScreenCaptureAccess func() bool
	cgRequestScreenCaptureAccess   func() bool
	cgMainDisplayID                func() uint32
	cgDisplayPixelsWide            func(uint32) uint64
	cgDisplayPixelsHigh            func(uint32) uint64

	dispatchQueueCreate func(string, uintptr) uintptr
	dispatchRelease     func(uintptr)
)

var (
	loadOnce sync.Once
	loadErr  error
)

// dlopen is a package seam so a test can force the load-failure branch.
var dlopen = func(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}

// load resolves every framework and C entry point exactly once.
func load() error {
	loadOnce.Do(func() { loadErr = doLoad() })
	return loadErr
}

func doLoad() error {
	if err := objc.Load(objc.Foundation, frameworkCoreMedia, frameworkCoreVideo,
		frameworkCoreGraphics, frameworkScreenCaptureKit); err != nil {
		return fmt.Errorf("screencapture: %w", err)
	}
	cm, err := dlopen(frameworkCoreMedia)
	if err != nil {
		return fmt.Errorf("screencapture: CoreMedia: %w", err)
	}
	cv, err := dlopen(frameworkCoreVideo)
	if err != nil {
		return fmt.Errorf("screencapture: CoreVideo: %w", err)
	}
	cg, err := dlopen(frameworkCoreGraphics)
	if err != nil {
		return fmt.Errorf("screencapture: CoreGraphics: %w", err)
	}
	sys, err := dlopen(objc.LibSystem)
	if err != nil {
		return fmt.Errorf("screencapture: libSystem: %w", err)
	}
	purego.RegisterLibFunc(&cmSampleBufferGetImageBuffer, cm, "CMSampleBufferGetImageBuffer")
	purego.RegisterLibFunc(&cmSampleBufferIsValid, cm, "CMSampleBufferIsValid")
	purego.RegisterLibFunc(&cvPixelBufferLockBaseAddress, cv, "CVPixelBufferLockBaseAddress")
	purego.RegisterLibFunc(&cvPixelBufferUnlockBaseAddress, cv, "CVPixelBufferUnlockBaseAddress")
	purego.RegisterLibFunc(&cvPixelBufferGetBaseAddress, cv, "CVPixelBufferGetBaseAddress")
	purego.RegisterLibFunc(&cvPixelBufferGetBytesPerRow, cv, "CVPixelBufferGetBytesPerRow")
	purego.RegisterLibFunc(&cvPixelBufferGetWidth, cv, "CVPixelBufferGetWidth")
	purego.RegisterLibFunc(&cvPixelBufferGetHeight, cv, "CVPixelBufferGetHeight")
	purego.RegisterLibFunc(&cvBufferRetain, cv, "CVBufferRetain")
	purego.RegisterLibFunc(&cvBufferRelease, cv, "CVBufferRelease")
	purego.RegisterLibFunc(&cgPreflightScreenCaptureAccess, cg, "CGPreflightScreenCaptureAccess")
	purego.RegisterLibFunc(&cgRequestScreenCaptureAccess, cg, "CGRequestScreenCaptureAccess")
	purego.RegisterLibFunc(&cgMainDisplayID, cg, "CGMainDisplayID")
	purego.RegisterLibFunc(&cgDisplayPixelsWide, cg, "CGDisplayPixelsWide")
	purego.RegisterLibFunc(&cgDisplayPixelsHigh, cg, "CGDisplayPixelsHigh")
	purego.RegisterLibFunc(&dispatchQueueCreate, sys, "dispatch_queue_create")
	purego.RegisterLibFunc(&dispatchRelease, sys, "dispatch_release")
	return nil
}

// cmTime is CoreMedia's CMTime, 24 bytes, passed BY VALUE to
// -[SCStreamConfiguration setMinimumFrameInterval:]. purego marshals it
// correctly on both arm64 (>16-byte aggregates go indirect) and amd64 (they go
// on the stack); a round trip through the setter and the getter is asserted in
// the darwin tests so the ABI is proven rather than assumed.
type cmTime struct {
	Value     int64
	Timescale int32
	Flags     uint32
	Epoch     int64
}

// cmTimeFlagValid is CMTime's kCMTimeFlags_Valid; without it the value is
// kCMTimeInvalid and ScreenCaptureKit ignores the frame-rate ceiling.
const cmTimeFlagValid = 1

// cgRect is CoreGraphics' CGRect, returned by value from -frame and
// -contentRect.
type cgRect struct{ X, Y, W, H float64 }

func (r cgRect) rect() Rect { return Rect{X: r.X, Y: r.Y, W: r.W, H: r.H} }

// ---------------------------------------------------------------------------
// Availability and permission.
// ---------------------------------------------------------------------------

// Available reports whether ScreenCaptureKit is present in this process. It is
// false on a macOS older than 12.3 and on any platform that is not macOS.
func Available() bool {
	if err := load(); err != nil {
		return false
	}
	return objc.GetClass("SCShareableContent") != 0 && objc.GetClass("SCStream") != 0
}

// Authorized reports whether this process may capture the screen RIGHT NOW,
// without starting a stream and without provoking a permission prompt. It is
// CGPreflightScreenCaptureAccess.
//
// A false answer does not mean capture is impossible: content owned by the
// calling process ([CurrentProcessShareable], and [CaptureWindow] on one of
// your own windows) is capturable with no grant at all.
func Authorized() bool {
	if err := load(); err != nil {
		return false
	}
	return cgPreflightScreenCaptureAccess()
}

// RequestAuthorization asks the system for the Screen Recording grant and
// reports whether the process holds it afterwards. The FIRST call from a
// process whose responsible application has never been asked raises the system
// prompt; once an application has been refused, the system records the refusal
// and every later call returns false immediately WITHOUT a prompt — from then
// on only the user, in System Settings > Privacy & Security > Screen & System
// Audio Recording, can change the answer. It is CGRequestScreenCaptureAccess.
func RequestAuthorization() bool {
	if err := load(); err != nil {
		return false
	}
	return cgRequestScreenCaptureAccess()
}

// ---------------------------------------------------------------------------
// Enumeration.
// ---------------------------------------------------------------------------

// Shareable returns everything this process may capture: every display, every
// window and every application owning one. It needs the Screen Recording
// grant; without it the call fails with an error matching
// [ErrPermissionDenied] rather than returning an empty list.
func Shareable(ctx context.Context) (*Content, error) {
	return shareable(ctx, false)
}

// CurrentProcessShareable returns only content owned by the CALLING process.
// It needs no permission whatsoever, which makes it the way to capture your
// own window — and the way this package's own live tests run on a machine
// where the Screen Recording grant is missing.
//
// Note that the list is what the window server attributes to this process,
// which on current macOS also includes a handful of system surfaces (the
// wallpaper, the menu bar) that every process is allowed to see.
func CurrentProcessShareable(ctx context.Context) (*Content, error) {
	return shareable(ctx, true)
}

// Displays is a shorthand for the display list from [Shareable].
func Displays(ctx context.Context) ([]Display, error) {
	c, err := Shareable(ctx)
	if err != nil {
		return nil, err
	}
	return c.Displays, nil
}

// Windows is a shorthand for the window list from [Shareable].
func Windows(ctx context.Context) ([]Window, error) {
	c, err := Shareable(ctx)
	if err != nil {
		return nil, err
	}
	return c.Windows, nil
}

// fetchContent runs the asynchronous +[SCShareableContent get…] class method
// and waits for its completion block. The block fires on one of
// ScreenCaptureKit's own dispatch queues, NOT on a run loop, so no run loop has
// to be pumped and the caller may be any goroutine — that was measured, not
// assumed.
//
// The returned SCShareableContent is RETAINED; the caller must release it. It
// is retained inside the block because the object handed to a completion block
// is autoreleased into that queue's pool and would be gone by the time this
// function returns.
func fetchContent(ctx context.Context, currentProcessOnly bool) (objc.ID, error) {
	if err := load(); err != nil {
		return 0, err
	}
	cls := objc.ClassID("SCShareableContent")
	if cls == 0 {
		return 0, ErrUnsupported
	}
	sel := objc.Sel("getShareableContentWithCompletionHandler:")
	op := "getShareableContent"
	if currentProcessOnly {
		sel = objc.Sel("getCurrentProcessShareableContentWithCompletionHandler:")
		op = "getCurrentProcessShareableContent"
		// getCurrentProcessShareableContent… arrived in macOS 14. On an older
		// system fall back to the permissioned call rather than messaging a
		// selector the class does not implement.
		if !objc.Send[bool](cls, objc.Sel("respondsToSelector:"), sel) {
			sel = objc.Sel("getShareableContentWithCompletionHandler:")
			op = "getShareableContent"
		}
	}

	done := make(chan struct{})
	var content, nsErr objc.ID
	var once sync.Once
	blk := newBlock(func(_ block, c objc.ID, e objc.ID) {
		// Retain before the block returns: both arguments are autoreleased.
		if c != 0 {
			c.Send(objc.Sel("retain"))
		}
		if e != 0 {
			e.Send(objc.Sel("retain"))
		}
		content, nsErr = c, e
		once.Do(func() { close(done) })
	})
	cls.Send(sel, blk)

	select {
	case <-done:
	case <-ctx.Done():
		// The block may still fire later and would then write into content /
		// nsErr. It is left registered rather than released precisely so that
		// late write lands somewhere valid; leaking one block per abandoned
		// enumeration is the safe trade against a use-after-free.
		return 0, fmt.Errorf("screencapture: %s: %w", op, ctx.Err())
	}
	blk.release()

	if nsErr != 0 {
		err := nsErrorOf(op, nsErr)
		nsErr.Send(objc.Sel("release"))
		if content != 0 {
			content.Send(objc.Sel("release"))
		}
		return 0, err
	}
	if content == 0 {
		return 0, fmt.Errorf("screencapture: %s: no content and no error from ScreenCaptureKit", op)
	}
	return content, nil
}

// withPool runs fn inside an NSAutoreleasePool on a PINNED OS thread.
//
// The pinning is not decoration. An autorelease pool must be drained on the
// same thread that created it, and a Go goroutine moves between OS threads
// whenever it parks — so wrapping a pool around anything that blocks on a
// channel crashes in -drain on an unrelated thread. That was not a theory: it
// segfaulted in exactly that spot the first time this package waited for
// -startCaptureWithCompletionHandler: inside a pool.
//
// The rule this package follows, therefore: fn must NEVER block. Every
// blocking wait happens outside a pool.
func withPool(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	objc.AutoreleasePool(fn)
}

// nsErrorOf turns an NSError from SCStreamErrorDomain into a [StreamError].
// -localizedDescription hands back an autoreleased NSString, so the read runs
// inside a pool: this is called from goroutines that have none of their own.
func nsErrorOf(op string, e objc.ID) error {
	var code int
	var msg string
	withPool(func() {
		code = objc.Send[int](e, objc.Sel("code"))
		msg = objc.GoString(e.Send(objc.Sel("localizedDescription")))
	})
	return newStreamError(op, code, msg)
}

// shareable enumerates and converts.
func shareable(ctx context.Context, currentProcessOnly bool) (*Content, error) {
	raw, err := fetchContent(ctx, currentProcessOnly)
	if err != nil {
		return nil, err
	}
	defer raw.Send(objc.Sel("release"))
	var out *Content
	withPool(func() { out = convertContent(raw) })
	return out, nil
}

// convertContent copies an SCShareableContent into plain Go values, so nothing
// Objective-C escapes the package.
func convertContent(raw objc.ID) *Content {
	c := &Content{}
	main := cgMainDisplayID()

	displays := raw.Send(objc.Sel("displays"))
	for i, n := 0, int(displays.Send(objc.Sel("count"))); i < n; i++ {
		d := displays.Send(objc.Sel("objectAtIndex:"), i)
		id := objc.Send[uint32](d, objc.Sel("displayID"))
		c.Displays = append(c.Displays, Display{
			ID:     id,
			Width:  objc.Send[int](d, objc.Sel("width")),
			Height: objc.Send[int](d, objc.Sel("height")),
			// SCDisplay reports POINTS. The native backing store comes from
			// CoreGraphics, which needs no permission, so a Retina display can
			// be captured at 1:1 without guessing a scale factor.
			PixelWidth:  int(cgDisplayPixelsWide(id)),
			PixelHeight: int(cgDisplayPixelsHigh(id)),
			Frame:       objc.Send[cgRect](d, objc.Sel("frame")).rect(),
			Main:        id == main,
		})
	}

	windows := raw.Send(objc.Sel("windows"))
	for i, n := 0, int(windows.Send(objc.Sel("count"))); i < n; i++ {
		w := windows.Send(objc.Sel("objectAtIndex:"), i)
		win := Window{
			ID:       objc.Send[uint32](w, objc.Sel("windowID")),
			Title:    objc.GoString(w.Send(objc.Sel("title"))),
			Frame:    objc.Send[cgRect](w, objc.Sel("frame")).rect(),
			Layer:    objc.Send[int](w, objc.Sel("windowLayer")),
			OnScreen: objc.Send[bool](w, objc.Sel("isOnScreen")),
		}
		// -isActive is macOS 14; on an older system leave it false rather than
		// messaging a selector that is not there.
		if objc.Send[bool](w, objc.Sel("respondsToSelector:"), objc.Sel("isActive")) {
			win.Active = objc.Send[bool](w, objc.Sel("isActive"))
		}
		if app := w.Send(objc.Sel("owningApplication")); app != 0 {
			win.AppName = objc.GoString(app.Send(objc.Sel("applicationName")))
			win.BundleID = objc.GoString(app.Send(objc.Sel("bundleIdentifier")))
			win.PID = objc.Send[int32](app, objc.Sel("processID"))
		}
		c.Windows = append(c.Windows, win)
	}

	apps := raw.Send(objc.Sel("applications"))
	for i, n := 0, int(apps.Send(objc.Sel("count"))); i < n; i++ {
		a := apps.Send(objc.Sel("objectAtIndex:"), i)
		c.Applications = append(c.Applications, Application{
			PID:      objc.Send[int32](a, objc.Sel("processID")),
			Name:     objc.GoString(a.Send(objc.Sel("applicationName"))),
			BundleID: objc.GoString(a.Send(objc.Sel("bundleIdentifier"))),
		})
	}
	return c
}

// ---------------------------------------------------------------------------
// The output class.
// ---------------------------------------------------------------------------

// Every stream shares ONE registered Objective-C class. purego turns each
// method into a C callback and never frees one, and the process allows only a
// bounded number, so registering a class per stream would be a leak with a
// hard ceiling. Instances are told apart through streamsByObj.
var (
	outputClassOnce sync.Once
	outputClass     objc.Class
	outputClassErr  error

	streamsMu    sync.RWMutex
	streamsByObj = map[objc.ID]*Stream{}
)

func lookupStream(obj objc.ID) *Stream {
	streamsMu.RLock()
	s := streamsByObj[obj]
	streamsMu.RUnlock()
	return s
}

// registerOutputClass defines the class that receives frames and stop
// notifications.
//
// It does NOT declare formal protocol conformance: objc_getProtocol returns
// nil for both SCStreamOutput and SCStreamDelegate on macOS 26, because
// ScreenCaptureKit's binary carries no protocol metadata for them (verified by
// walking objc_copyProtocolList — the only SC* protocols present are the
// content-sharing XPC ones). -conformsToProtocol: is therefore overridden to
// answer YES, which is enough for -addStreamOutput:type:sampleHandlerQueue:error:
// to accept the object; that call was measured returning YES with a nil error.
func registerOutputClass() (objc.Class, error) {
	outputClassOnce.Do(func() {
		outputClass, outputClassErr = objc.RegisterClass(
			"GoMacOSScreenCaptureOutput", objc.GetClass("NSObject"), []objc.MethodDef{
				{Cmd: objc.Sel("stream:didOutputSampleBuffer:ofType:"), Fn: onSampleBuffer},
				{Cmd: objc.Sel("stream:didStopWithError:"), Fn: onDidStop},
				{Cmd: objc.Sel("conformsToProtocol:"), Fn: onConformsToProtocol},
			})
	})
	return outputClass, outputClassErr
}

// onSampleBuffer is -stream:didOutputSampleBuffer:ofType:. It runs on the
// stream's own serial dispatch queue, on a thread Go does not own.
func onSampleBuffer(self objc.ID, _ objc.SEL, _ objc.ID, sbuf uintptr, typ int) {
	if typ != outputTypeScreen {
		return
	}
	if s := lookupStream(self); s != nil {
		s.deliver(sbuf)
	}
}

// onDidStop is -stream:didStopWithError:, the SCStreamDelegate callback the
// system uses to say it tore the stream down behind our back (the user hit
// "stop sharing", the display went away, the process lost its grant).
func onDidStop(self objc.ID, _ objc.SEL, _ objc.ID, e objc.ID) {
	s := lookupStream(self)
	if s == nil {
		return
	}
	var err error = ErrClosed
	if e != 0 {
		err = nsErrorOf("stream", e)
	}
	s.mu.Lock()
	if s.stopErr == nil {
		s.stopErr = err
	}
	s.mu.Unlock()
	s.signal()
}

// onConformsToProtocol: see registerOutputClass.
func onConformsToProtocol(_ objc.ID, _ objc.SEL, _ uintptr) bool { return true }

// ---------------------------------------------------------------------------
// Stream.
// ---------------------------------------------------------------------------

// pixbuf is one retained, locked CVPixelBuffer and the Go view of its bytes.
// The buffer stays LOCKED for as long as this package holds it: unlocking is
// what invalidates the base address, so the lock is the borrow.
type pixbuf struct {
	pb            uintptr
	pix           []byte
	width, height int
	stride        int
	seq           uint64
	at            time.Time
}

func (p pixbuf) frame() Frame {
	return Frame{Pix: p.pix, Width: p.width, Height: p.height,
		Stride: p.stride, Seq: p.seq, At: p.at}
}

// release unlocks and releases the pixel buffer, handing it back to the
// stream's pool.
func (p *pixbuf) release() {
	if p.pb == 0 {
		return
	}
	cvPixelBufferUnlockBaseAddress(p.pb, cvReadOnly)
	cvBufferRelease(p.pb)
	*p = pixbuf{}
}

// Stream is a live capture. It is safe to use from several goroutines.
type Stream struct {
	opt    Options
	source string

	mu         sync.Mutex
	closed     bool
	pending    pixbuf // newest frame, not yet handed out
	current    pixbuf // frame currently lent to the consumer
	seq        uint64
	stats      Stats
	stopErr    error
	lastFrame  time.Time
	fresh      chan struct{}
	closeOnce  sync.Once
	closeError error

	// Objective-C state, released by Close.
	obj    objc.ID // our output/delegate instance
	stream objc.ID
	filter objc.ID
	cfg    objc.ID
	queue  uintptr
}

// Options returns the options this stream actually runs with, every zero field
// resolved to the value in use.
func (s *Stream) Options() Options { return s.opt }

// Source names what is being captured, for logs and errors.
func (s *Stream) Source() string { return s.source }

// signal wakes one waiter in WaitFrame without ever blocking the caller.
func (s *Stream) signal() {
	select {
	case s.fresh <- struct{}{}:
	default:
	}
}

// deliver is the hot path: it runs on the capture queue for every frame.
func (s *Stream) deliver(sbuf uintptr) {
	if sbuf == 0 || !cmSampleBufferIsValid(sbuf) {
		return
	}
	pb := cmSampleBufferGetImageBuffer(sbuf)
	if pb == 0 {
		// A callback with no image buffer is ScreenCaptureKit saying the
		// content did not change (SCFrameStatusIdle). It is normal, not an
		// error, and it is why a still screen produces almost no frames.
		s.mu.Lock()
		s.stats.Idle++
		s.mu.Unlock()
		return
	}
	// Retain and lock BEFORE publishing: the sample buffer is released as soon
	// as this callback returns, and with it the only other reference.
	cvBufferRetain(pb)
	if cvPixelBufferLockBaseAddress(pb, cvReadOnly) != 0 {
		cvBufferRelease(pb)
		return
	}
	base := cvPixelBufferGetBaseAddress(pb)
	w := int(cvPixelBufferGetWidth(pb))
	h := int(cvPixelBufferGetHeight(pb))
	stride := int(cvPixelBufferGetBytesPerRow(pb))
	if base == nil || w <= 0 || h <= 0 || stride < w*4 {
		cvPixelBufferUnlockBaseAddress(pb, cvReadOnly)
		cvBufferRelease(pb)
		return
	}
	now := time.Now()
	nb := pixbuf{
		pb: pb,
		// unsafe.Slice over window-server memory. It carries no Go pointers,
		// so the collector never scans it; its lifetime is the lock above.
		pix:    unsafe.Slice((*byte)(base), stride*h),
		width:  w,
		height: h,
		stride: stride,
		at:     now,
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		nb.release()
		return
	}
	s.seq++
	nb.seq = s.seq
	if s.pending.pb != 0 {
		// The consumer never asked for the previous frame. Drop it — the point
		// of this package is that the consumer gets the NEWEST frame, not a
		// backlog.
		s.pending.release()
		s.stats.Superseded++
	}
	s.pending = nb
	s.stats.Frames++
	if !s.lastFrame.IsZero() {
		s.stats.Interval = now.Sub(s.lastFrame)
	}
	s.lastFrame = now
	s.stats.Last = now
	s.mu.Unlock()
	s.signal()
}

// Frame returns the most recent captured frame and whether it is NEWER than
// the one the previous call returned.
//
// The returned Frame.Pix is BORROWED: it aliases the window server's own
// surface and stays valid only until the next Frame, WaitFrame or Close on
// this stream. In steady state this call performs no allocation and copies
// nothing.
//
// Before the first frame arrives it returns the zero Frame and false; check
// [Frame.Valid].
func (s *Stream) Frame() (Frame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending.pb != 0 {
		s.current.release()
		s.current = s.pending
		s.pending = pixbuf{}
		return s.current.frame(), true
	}
	return s.current.frame(), false
}

// WaitFrame blocks until a frame NEWER than the last one handed out arrives,
// and returns it on the same borrowed terms as [Stream.Frame].
//
// It reports an error wrapping [ErrNoFrame] when ctx expires first. That is
// not necessarily a malfunction: ScreenCaptureKit emits nothing at all while
// the captured content is motionless.
func (s *Stream) WaitFrame(ctx context.Context) (Frame, error) {
	for {
		s.mu.Lock()
		closed, stopErr := s.closed, s.stopErr
		s.mu.Unlock()
		if closed {
			return Frame{}, ErrClosed
		}
		if stopErr != nil {
			return Frame{}, stopErr
		}
		if f, fresh := s.Frame(); fresh {
			return f, nil
		}
		select {
		case <-s.fresh:
		case <-ctx.Done():
			return Frame{}, fmt.Errorf("%w: %w", ErrNoFrame, ctx.Err())
		}
	}
}

// Stats returns a snapshot of what the stream has seen.
func (s *Stream) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Err reports why the SYSTEM stopped the stream, if it did — the user revoked
// sharing, the display went away, the grant was withdrawn. It is nil for a
// healthy stream and for one the caller closed itself.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.stopErr
}

// Close stops the capture and releases everything. It is idempotent and safe
// to call from any goroutine, including concurrently with [Stream.Frame].
//
// Every Frame previously handed out is INVALID once Close returns.
func (s *Stream) Close() error {
	s.closeOnce.Do(func() { s.closeError = s.doClose() })
	return s.closeError
}

func (s *Stream) doClose() error {
	// Mark closed first, so a callback already in flight drops its frame
	// instead of publishing into a stream that is going away.
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	// Then unregister, so no NEW callback can even find this stream.
	streamsMu.Lock()
	delete(streamsByObj, s.obj)
	streamsMu.Unlock()

	var stopErr error
	if s.stream != 0 {
		done := make(chan struct{})
		var once sync.Once
		var nsErr objc.ID
		blk := newBlock(func(_ block, e objc.ID) {
			if e != 0 {
				e.Send(objc.Sel("retain"))
			}
			nsErr = e
			once.Do(func() { close(done) })
		})
		s.stream.Send(objc.Sel("stopCaptureWithCompletionHandler:"), blk)
		select {
		case <-done:
			blk.release()
			if nsErr != 0 {
				// -3808 is "you stopped a stream that was not running", which
				// on a Close is a statement of fact, not a failure.
				if code := objc.Send[int](nsErr, objc.Sel("code")); code != -3808 {
					stopErr = nsErrorOf("stopCapture", nsErr)
				}
				nsErr.Send(objc.Sel("release"))
			}
		case <-time.After(stopTimeout):
			// The block is deliberately not released: it may still fire.
			stopErr = fmt.Errorf("screencapture: stopCapture: %w",
				context.DeadlineExceeded)
		}
	}

	s.mu.Lock()
	s.pending.release()
	s.current.release()
	s.mu.Unlock()

	for _, o := range []*objc.ID{&s.stream, &s.filter, &s.cfg, &s.obj} {
		if *o != 0 {
			(*o).Send(objc.Sel("release"))
			*o = 0
		}
	}
	if s.queue != 0 {
		dispatchRelease(s.queue)
		s.queue = 0
	}
	return stopErr
}

// ---------------------------------------------------------------------------
// Opening a stream.
// ---------------------------------------------------------------------------

// CaptureDisplay opens a stream on a display. It needs the Screen Recording
// grant; without it the call fails with an error matching [ErrPermissionDenied].
//
// opt.ExcludeWindows keeps the named windows out of the capture, which is how
// a compositor avoids capturing its own overlay.
func CaptureDisplay(ctx context.Context, d Display, opt Options) (*Stream, error) {
	if err := opt.Validate(); err != nil {
		return nil, err
	}
	raw, err := fetchContent(ctx, false)
	if err != nil {
		return nil, err
	}
	defer raw.Send(objc.Sel("release"))

	// Building the filter touches autoreleased objects and must sit in a pool;
	// starting the stream BLOCKS and must not. Hence the two phases.
	var filter objc.ID
	var resolved Options
	var openErr error
	withPool(func() {
		scDisplay, excluded := findDisplay(raw, d.ID, opt.ExcludeWindows)
		if scDisplay == 0 {
			openErr = fmt.Errorf("%w: display %d", ErrNotFound, d.ID)
			return
		}
		// Fall back to CoreGraphics for the native size when the caller passed
		// a Display it did not get from this package (a bare ID, say).
		nw, nh := d.PixelWidth, d.PixelHeight
		if nw <= 0 || nh <= 0 {
			nw, nh = int(cgDisplayPixelsWide(d.ID)), int(cgDisplayPixelsHigh(d.ID))
		}
		if resolved, openErr = opt.resolve(nw, nh); openErr != nil {
			return
		}
		filter = objc.ClassID("SCContentFilter").Send(objc.Sel("alloc")).
			Send(objc.Sel("initWithDisplay:excludingWindows:"), scDisplay, excluded)
		if filter == 0 {
			openErr = fmt.Errorf("screencapture: SCContentFilter: display %d refused", d.ID)
		}
	})
	if openErr != nil {
		return nil, openErr
	}
	return newStream(ctx, filter, resolved, fmt.Sprintf("display %d", d.ID))
}

// CaptureWindow opens a stream on a single window, at the window's own size,
// without whatever overlaps it.
//
// A window belonging to the CALLING process is capturable with NO permission
// at all; this function tries the permissioned enumeration first and falls
// back to the current-process one, so capturing your own window works on a
// machine where the Screen Recording grant was never given.
func CaptureWindow(ctx context.Context, w Window, opt Options) (*Stream, error) {
	if err := opt.Validate(); err != nil {
		return nil, err
	}
	raw, err := fetchContent(ctx, false)
	if err != nil {
		raw, err = fetchContent(ctx, true)
		if err != nil {
			return nil, err
		}
	}
	defer raw.Send(objc.Sel("release"))

	var filter objc.ID
	var resolved Options
	var openErr error
	withPool(func() {
		scWindow := findWindow(raw, w.ID)
		if scWindow == 0 {
			openErr = fmt.Errorf("%w: window %d", ErrNotFound, w.ID)
			return
		}
		filter = objc.ClassID("SCContentFilter").Send(objc.Sel("alloc")).
			Send(objc.Sel("initWithDesktopIndependentWindow:"), scWindow)
		if filter == 0 {
			openErr = fmt.Errorf("screencapture: SCContentFilter: window %d refused", w.ID)
			return
		}
		// A window's native pixel size is its point frame times the scale of
		// the display it sits on. SCContentFilter reports both from macOS 14
		// on: contentRect in points, pointPixelScale as the factor. Both are
		// treated as hints — pointPixelScale is a float, and a nonsensical
		// value falls back to 1 rather than asking for a nonsense frame size.
		rect := objc.Send[cgRect](filter, objc.Sel("contentRect"))
		if rect.Empty() {
			rect = cgRect{W: w.Frame.W, H: w.Frame.H}
		}
		scale := float64(objc.Send[float32](filter, objc.Sel("pointPixelScale")))
		if scale < 1 || scale > 8 {
			scale = 1
		}
		resolved, openErr = opt.resolve(int(rect.W*scale+0.5), int(rect.H*scale+0.5))
	})
	if openErr != nil {
		if filter != 0 {
			filter.Send(objc.Sel("release"))
		}
		return nil, openErr
	}
	return newStream(ctx, filter, resolved, fmt.Sprintf("window %d", w.ID))
}

// Empty reports whether the CGRect encloses no area.
func (r cgRect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// findDisplay locates the SCDisplay with the given ID and builds the
// NSArray<SCWindow*> of windows to exclude from it.
func findDisplay(raw objc.ID, id uint32, exclude []uint32) (objc.ID, objc.ID) {
	displays := raw.Send(objc.Sel("displays"))
	var found objc.ID
	for i, n := 0, int(displays.Send(objc.Sel("count"))); i < n; i++ {
		d := displays.Send(objc.Sel("objectAtIndex:"), i)
		if objc.Send[uint32](d, objc.Sel("displayID")) == id {
			found = d
			break
		}
	}
	excluded := objc.ClassID("NSMutableArray").Send(objc.Sel("array"))
	if len(exclude) > 0 {
		want := make(map[uint32]bool, len(exclude))
		for _, e := range exclude {
			want[e] = true
		}
		windows := raw.Send(objc.Sel("windows"))
		for i, n := 0, int(windows.Send(objc.Sel("count"))); i < n; i++ {
			w := windows.Send(objc.Sel("objectAtIndex:"), i)
			if want[objc.Send[uint32](w, objc.Sel("windowID"))] {
				excluded.Send(objc.Sel("addObject:"), w)
			}
		}
	}
	return found, excluded
}

// findWindow locates the SCWindow with the given CGWindowID.
func findWindow(raw objc.ID, id uint32) objc.ID {
	windows := raw.Send(objc.Sel("windows"))
	for i, n := 0, int(windows.Send(objc.Sel("count"))); i < n; i++ {
		w := windows.Send(objc.Sel("objectAtIndex:"), i)
		if objc.Send[uint32](w, objc.Sel("windowID")) == id {
			return w
		}
	}
	return 0
}

// newStream configures and starts a stream on an already-built filter. It
// takes ownership of filter (which arrived with a +1 retain from alloc/init).
func newStream(ctx context.Context, filter objc.ID, opt Options, source string) (*Stream, error) {
	cls, err := registerOutputClass()
	if err != nil {
		filter.Send(objc.Sel("release"))
		return nil, fmt.Errorf("screencapture: register output class: %w", err)
	}

	cfg := objc.ClassID("SCStreamConfiguration").Send(objc.Sel("alloc")).Send(objc.Sel("init"))
	cfg.Send(objc.Sel("setWidth:"), uint64(opt.Width))
	cfg.Send(objc.Sel("setHeight:"), uint64(opt.Height))
	cfg.Send(objc.Sel("setPixelFormat:"), uint32(FormatBGRA))
	cfg.Send(objc.Sel("setShowsCursor:"), opt.ShowsCursor)
	cfg.Send(objc.Sel("setQueueDepth:"), opt.QueueDepth)
	cfg.Send(objc.Sel("setScalesToFit:"), opt.ScalesToFit)
	v, ts := frameInterval(opt.FPS)
	cfg.Send(objc.Sel("setMinimumFrameInterval:"),
		cmTime{Value: v, Timescale: ts, Flags: cmTimeFlagValid})

	obj := objc.ID(cls).Send(objc.Sel("alloc")).Send(objc.Sel("init"))
	queue := dispatchQueueCreate("com.go-macos.screencapture", 0)

	s := &Stream{
		opt:    opt,
		source: source,
		fresh:  make(chan struct{}, 1),
		obj:    obj,
		filter: filter,
		cfg:    cfg,
		queue:  queue,
	}

	// Register BEFORE the stream can produce a frame.
	streamsMu.Lock()
	streamsByObj[obj] = s
	streamsMu.Unlock()

	stream := objc.ClassID("SCStream").Send(objc.Sel("alloc")).
		Send(objc.Sel("initWithFilter:configuration:delegate:"), filter, cfg, obj)
	if stream == 0 {
		s.Close()
		return nil, fmt.Errorf("screencapture: SCStream: %s refused the filter", source)
	}
	s.stream = stream

	var addErr objc.ID
	ok := objc.Send[bool](stream, objc.Sel("addStreamOutput:type:sampleHandlerQueue:error:"),
		obj, outputTypeScreen, queue, &addErr)
	if !ok {
		err := fmt.Errorf("screencapture: addStreamOutput: %s", source)
		if addErr != 0 {
			err = nsErrorOf("addStreamOutput", addErr)
		}
		s.Close()
		return nil, err
	}

	if err := s.start(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// start runs -startCaptureWithCompletionHandler: and waits for its answer.
func (s *Stream) start(ctx context.Context) error {
	done := make(chan struct{})
	var once sync.Once
	var nsErr objc.ID
	blk := newBlock(func(_ block, e objc.ID) {
		if e != 0 {
			e.Send(objc.Sel("retain"))
		}
		nsErr = e
		once.Do(func() { close(done) })
	})
	s.stream.Send(objc.Sel("startCaptureWithCompletionHandler:"), blk)
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("screencapture: startCapture: %w", ctx.Err())
	}
	blk.release()
	if nsErr != 0 {
		err := nsErrorOf("startCapture", nsErr)
		nsErr.Send(objc.Sel("release"))
		return err
	}
	runtime.KeepAlive(s)
	return nil
}
