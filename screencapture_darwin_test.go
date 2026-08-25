// Copyright (c) the go-macos/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

// Darwin tests that need NO display and NO Screen Recording grant, so they run
// unattended on a CI runner. Everything here talks to the real Objective-C
// runtime and the real ScreenCaptureKit — nothing is mocked — but nothing here
// opens a stream on someone else's content. The live proof that frames arrive
// and change is in live_darwin_test.go, behind -tags=integration.
package screencapture

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-macos/objc"
)

func TestDarwinLoadAndAvailable(t *testing.T) {
	if err := load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !Available() {
		t.Fatal("Available() is false on a macOS that ships ScreenCaptureKit")
	}
	// The classes this package messages must all be present. A missing one
	// would otherwise show up much later as a nil-object no-op.
	for _, name := range []string{"SCShareableContent", "SCStream", "SCStreamConfiguration",
		"SCContentFilter", "SCDisplay", "SCWindow", "SCRunningApplication"} {
		if objc.GetClass(name) == 0 {
			t.Errorf("class %s is absent from the runtime", name)
		}
	}
	// Authorized must not panic and must agree with itself.
	t.Logf("Authorized() = %v", Authorized())
	if Authorized() != Authorized() {
		t.Error("Authorized() is not stable across two calls")
	}
}

// TestDarwinLoadFailurePaths drives every dlopen branch in doLoad by making one
// library at a time refuse to open.
func TestDarwinLoadFailurePaths(t *testing.T) {
	real := dlopen
	t.Cleanup(func() { dlopen = real })
	boom := errors.New("refused")
	for _, tc := range []struct{ fail, want string }{
		{frameworkCoreMedia, "CoreMedia"},
		{frameworkCoreVideo, "CoreVideo"},
		{frameworkCoreGraphics, "CoreGraphics"},
		{objc.LibSystem, "libSystem"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			dlopen = func(path string) (uintptr, error) {
				if path == tc.fail {
					return 0, boom
				}
				return real(path)
			}
			err := doLoad()
			if err == nil {
				t.Fatalf("doLoad() succeeded with %s refusing to open", tc.fail)
			}
			if !errors.Is(err, boom) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("doLoad() = %v, want it to name %s and wrap the cause", err, tc.want)
			}
		})
	}
	// Restore and prove the real load still works afterwards.
	dlopen = real
	if err := doLoad(); err != nil {
		t.Fatalf("doLoad after restoring the seam: %v", err)
	}
}

// TestDarwinCMTimeABI is the ABI proof for the one struct this package passes
// BY VALUE across the Objective-C boundary.
//
// -[SCStreamConfiguration setMinimumFrameInterval:] takes a 24-byte CMTime by
// value, which arm64 passes indirectly and amd64 passes on the stack. If the
// marshalling were wrong the frame-rate ceiling would be silently ignored —
// there is no error, just a stream running at the wrong rate. Setting it and
// reading it back through the real class is the only honest check.
func TestDarwinCMTimeABI(t *testing.T) {
	if err := load(); err != nil {
		t.Fatal(err)
	}
	var got cmTime
	var w, h uint64
	var format uint32
	var depth int
	withPool(func() {
		cfg := objc.ClassID("SCStreamConfiguration").Send(objc.Sel("alloc")).Send(objc.Sel("init"))
		defer cfg.Send(objc.Sel("release"))
		v, ts := frameInterval(60)
		cfg.Send(objc.Sel("setMinimumFrameInterval:"),
			cmTime{Value: v, Timescale: ts, Flags: cmTimeFlagValid})
		cfg.Send(objc.Sel("setWidth:"), uint64(1920))
		cfg.Send(objc.Sel("setHeight:"), uint64(1080))
		cfg.Send(objc.Sel("setPixelFormat:"), uint32(FormatBGRA))
		cfg.Send(objc.Sel("setQueueDepth:"), 6)
		got = objc.Send[cmTime](cfg, objc.Sel("minimumFrameInterval"))
		w = objc.Send[uint64](cfg, objc.Sel("width"))
		h = objc.Send[uint64](cfg, objc.Sel("height"))
		format = objc.Send[uint32](cfg, objc.Sel("pixelFormat"))
		depth = objc.Send[int](cfg, objc.Sel("queueDepth"))
	})
	wantV, wantTS := frameInterval(60)
	if got.Value != wantV || got.Timescale != wantTS || got.Flags&cmTimeFlagValid == 0 {
		t.Errorf("minimumFrameInterval round-tripped as %+v, want Value=%d Timescale=%d and the valid flag",
			got, wantV, wantTS)
	}
	if w != 1920 || h != 1080 {
		t.Errorf("size round-tripped as %dx%d, want 1920x1080", w, h)
	}
	if PixelFormat(format) != FormatBGRA {
		t.Errorf("pixelFormat round-tripped as %s, want %s", PixelFormat(format), FormatBGRA)
	}
	if depth != 6 {
		t.Errorf("queueDepth round-tripped as %d, want 6", depth)
	}
}

// TestDarwinOutputClass proves the runtime class this package registers really
// implements the three selectors ScreenCaptureKit will send it. The protocols
// themselves are NOT in the runtime (see registerOutputClass), so
// respondsToSelector: is the only thing that can be checked here.
func TestDarwinOutputClass(t *testing.T) {
	cls, err := registerOutputClass()
	if err != nil {
		t.Fatalf("registerOutputClass: %v", err)
	}
	if cls == 0 {
		t.Fatal("registerOutputClass returned the nil class")
	}
	// It is registered once for the whole process.
	again, err := registerOutputClass()
	if err != nil || again != cls {
		t.Errorf("registerOutputClass is not idempotent: %v, %v", again, err)
	}
	var obj objc.ID
	withPool(func() {
		obj = objc.ID(cls).Send(objc.Sel("alloc")).Send(objc.Sel("init"))
	})
	if obj == 0 {
		t.Fatal("could not instantiate the output class")
	}
	defer obj.Send(objc.Sel("release"))
	for _, sel := range []string{"stream:didOutputSampleBuffer:ofType:", "stream:didStopWithError:",
		"conformsToProtocol:"} {
		if !objc.Send[bool](obj, objc.Sel("respondsToSelector:"), objc.Sel(sel)) {
			t.Errorf("the output class does not respond to %s", sel)
		}
	}
	// The overridden conformsToProtocol: is what lets
	// -addStreamOutput:type:sampleHandlerQueue:error: accept the object even
	// though objc_getProtocol("SCStreamOutput") is nil on this OS.
	if !objc.Send[bool](obj, objc.Sel("conformsToProtocol:"), uintptr(0)) {
		t.Error("conformsToProtocol: answered NO; addStreamOutput would refuse the object")
	}
	if objc.GetProtocol("SCStreamOutput") != nil {
		t.Log("SCStreamOutput is now a real protocol in the runtime; " +
			"the conformsToProtocol: override could be replaced by RegisterClassWithProtocols")
	}
}

// TestDarwinCallbacksIgnoreUnknownReceivers proves the callbacks cannot be made
// to write into a stream that no longer exists — the lifetime hazard of a
// registered Objective-C callback outliving its Go object.
func TestDarwinCallbacksIgnoreUnknownReceivers(t *testing.T) {
	if err := load(); err != nil {
		t.Fatal(err)
	}
	const ghost = objc.ID(0xdeadbeef)
	if lookupStream(ghost) != nil {
		t.Fatal("a stream is registered for an object that was never created")
	}
	// Neither callback may touch anything for an unknown receiver.
	onSampleBuffer(ghost, 0, 0, 0, outputTypeScreen)
	onSampleBuffer(ghost, 0, 0, 0, 1) // audio: ignored before the lookup
	onDidStop(ghost, 0, 0, 0)
	if !onConformsToProtocol(ghost, 0, 0) {
		t.Error("conformsToProtocol: must answer YES")
	}
}

// newTestStream builds a Stream with no Objective-C state behind it, so the
// bookkeeping — freshness, closure, statistics — can be driven deterministically.
func newTestStream() *Stream {
	return &Stream{
		opt:    Options{Width: 4, Height: 3, FPS: 60, QueueDepth: 6},
		source: "test",
		fresh:  make(chan struct{}, 1),
	}
}

func TestDarwinStreamBookkeeping(t *testing.T) {
	s := newTestStream()
	if s.Options().Width != 4 || s.Source() != "test" {
		t.Errorf("Options/Source did not survive construction: %+v %q", s.Options(), s.Source())
	}
	// Before any frame.
	if f, fresh := s.Frame(); fresh || f.Valid() {
		t.Errorf("Frame() before any capture = %+v, fresh=%v; want the zero frame and false", f, fresh)
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err() on a healthy stream = %v, want nil", err)
	}
	if st := s.Stats(); st.Frames != 0 || st.FPS() != 0 {
		t.Errorf("Stats() before any frame = %+v", st)
	}

	// signal() must never block, however often it is called.
	for i := 0; i < 10; i++ {
		s.signal()
	}

	// A delivery callback with no sample buffer, and one with an invalid one,
	// must both be no-ops rather than crashes.
	s.deliver(0)
	if st := s.Stats(); st.Frames != 0 || st.Idle != 0 {
		t.Errorf("deliver(0) changed the statistics: %+v", st)
	}

	// WaitFrame with an already-expired context reports ErrNoFrame, and says
	// why underneath.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err := s.WaitFrame(ctx)
	if !errors.Is(err, ErrNoFrame) {
		t.Errorf("WaitFrame past its deadline = %v, want ErrNoFrame", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitFrame error = %v, want it to carry the context cause too", err)
	}
}

func TestDarwinStreamStopError(t *testing.T) {
	s := newTestStream()
	// The system tearing the stream down behind our back.
	onDidStopInto(s, nil)
	if err := s.Err(); !errors.Is(err, ErrClosed) {
		t.Errorf("Err() after a stop with no NSError = %v, want ErrClosed", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.WaitFrame(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("WaitFrame on a stopped stream = %v, want the stop reason", err)
	}
	// A second stop must not overwrite the first reason.
	onDidStopInto(s, errors.New("later"))
	if err := s.Err(); !errors.Is(err, ErrClosed) {
		t.Errorf("the stop reason was overwritten: %v", err)
	}
}

// onDidStopInto is the body of onDidStop with the receiver already resolved, so
// the bookkeeping can be tested without an Objective-C instance.
func onDidStopInto(s *Stream, err error) {
	if err == nil {
		err = ErrClosed
	}
	s.mu.Lock()
	if s.stopErr == nil {
		s.stopErr = err
	}
	s.mu.Unlock()
	s.signal()
}

func TestDarwinStreamCloseIsIdempotent(t *testing.T) {
	s := newTestStream()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+2, err)
		}
	}
	// Everything reports closed afterwards.
	if f, fresh := s.Frame(); fresh || f.Valid() {
		t.Errorf("Frame() after Close = %+v, fresh=%v", f, fresh)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.WaitFrame(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("WaitFrame after Close = %v, want ErrClosed", err)
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err() after our own Close = %v, want nil — we stopped it, nothing failed", err)
	}
	// A delivery that races Close must drop its frame rather than publish it.
	s.deliver(0)
}

// TestDarwinCurrentProcessShareable exercises the permission-free enumeration
// and the whole SCShareableContent -> Go conversion. It needs no display: a
// headless runner simply lists fewer surfaces.
func TestDarwinCurrentProcessShareable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := CurrentProcessShareable(ctx)
	if err != nil {
		t.Fatalf("CurrentProcessShareable must never need a grant: %v", err)
	}
	t.Logf("current process may capture %d display(s), %d window(s), %d application(s)",
		len(c.Displays), len(c.Windows), len(c.Applications))
	for _, w := range c.Windows {
		if w.ID == 0 {
			t.Error("a window came back with a zero CGWindowID")
		}
		if _, err := c.Window(w.ID); err != nil {
			t.Errorf("window %d is in the list but Content.Window cannot find it: %v", w.ID, err)
		}
	}
	for _, d := range c.Displays {
		if d.ID == 0 {
			t.Error("a display came back with a zero CGDirectDisplayID")
		}
		// The two size sources must agree about the aspect ratio: SCDisplay
		// reports points, CoreGraphics reports pixels, and the scale between
		// them is a small integer on every Mac.
		if sc := d.Scale(); sc < 1 || sc > 4 {
			t.Errorf("%s implies a backing scale of %g", d, sc)
		}
	}
}

// TestDarwinShareableWithoutGrant documents the denied path. On a machine WITH
// the grant it must succeed; on one without it must fail with a named error
// that says what to do — never with a bare nil or a panic.
func TestDarwinShareableWithoutGrant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Shareable(ctx)
	switch {
	case err == nil:
		if !Authorized() {
			t.Error("Shareable succeeded but Authorized() says the grant is missing")
		}
		if len(c.Displays) == 0 {
			t.Error("Shareable succeeded and listed no display at all")
		}
		t.Logf("this machine HAS the Screen Recording grant: %d displays", len(c.Displays))
	case errors.Is(err, ErrPermissionDenied):
		var se *StreamError
		if !errors.As(err, &se) {
			t.Fatalf("the denial is not a *StreamError: %T %v", err, err)
		}
		if se.Code != errUserDeclined && se.Code != errMissingEntitlements {
			t.Errorf("denial code = %d, want %d or %d", se.Code, errUserDeclined, errMissingEntitlements)
		}
		if !strings.Contains(err.Error(), "System Settings") {
			t.Errorf("the denial does not tell the user what to do:\n%s", err)
		}
		if Authorized() {
			t.Error("Shareable was denied but Authorized() says the grant is held")
		}
		t.Logf("this machine LACKS the Screen Recording grant; the error reads:\n%s", err)
	default:
		t.Fatalf("Shareable failed for an unexpected reason: %v", err)
	}
}

// TestDarwinFetchContentCancelled proves an abandoned enumeration returns
// rather than hanging, and reports the context's own cause.
func TestDarwinFetchContentCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetchContent(ctx, true); !errors.Is(err, context.Canceled) {
		t.Errorf("fetchContent with a cancelled context = %v, want context.Canceled", err)
	}
}

// TestDarwinCaptureRejectsBadOptions proves option validation happens before
// anything Objective-C is touched, so a bad option is a clean error even with
// no grant and no display.
func TestDarwinCaptureRejectsBadOptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bad := Options{Width: 100} // height missing
	if _, err := CaptureDisplay(ctx, Display{ID: 1}, bad); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("CaptureDisplay with bad options = %v, want ErrInvalidOption", err)
	}
	if _, err := CaptureWindow(ctx, Window{ID: 1}, bad); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("CaptureWindow with bad options = %v, want ErrInvalidOption", err)
	}
}

// TestDarwinCaptureUnknownWindow proves a window ID that names nothing produces
// ErrNotFound rather than a nil dereference. It goes through the
// current-process fallback, so it needs no grant.
func TestDarwinCaptureUnknownWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := CaptureWindow(ctx, Window{ID: 0xfffffff0}, Options{})
	if err == nil {
		t.Fatal("capturing a window that does not exist succeeded")
	}
	if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("CaptureWindow of an unknown window = %v, want ErrNotFound", err)
	}
}

// TestDarwinCaptureUnknownDisplay is the same for a display. Without the grant
// the enumeration fails first, which is itself the named permission error.
func TestDarwinCaptureUnknownDisplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := CaptureDisplay(ctx, Display{ID: 0xfffffff0}, Options{})
	if err == nil {
		t.Fatal("capturing a display that does not exist succeeded")
	}
	if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("CaptureDisplay of an unknown display = %v, want ErrNotFound or ErrPermissionDenied", err)
	}
	t.Logf("unknown display: %v", err)
}

// TestDarwinPixbufReleaseIsSafe proves the borrowed-buffer bookkeeping tolerates
// a release of nothing, which is what Close does on a stream that never
// produced a frame.
func TestDarwinPixbufReleaseIsSafe(t *testing.T) {
	var p pixbuf
	p.release()
	p.release()
	if f := p.frame(); f.Valid() {
		t.Errorf("an empty pixbuf produced a valid frame: %+v", f)
	}
}

// TestDarwinWithPoolNesting proves withPool survives being nested and that it
// leaves the thread unpinned afterwards.
func TestDarwinWithPoolNesting(t *testing.T) {
	var inner bool
	withPool(func() {
		withPool(func() {
			s := objc.GoString(objc.NSString("nested"))
			inner = s == "nested"
		})
	})
	if !inner {
		t.Error("a nested autorelease pool did not round-trip an NSString")
	}
}

// TestDarwinRectConversion covers the CGRect bridge type.
func TestDarwinRectConversion(t *testing.T) {
	r := cgRect{1, 2, 3, 4}.rect()
	if r != (Rect{1, 2, 3, 4}) {
		t.Errorf("cgRect.rect() = %v", r)
	}
	if (cgRect{W: 3, H: 4}).Empty() {
		t.Error("a 3x4 CGRect is not empty")
	}
	for _, e := range []cgRect{{}, {W: 1}, {H: 1}} {
		if !e.Empty() {
			t.Errorf("%+v should be empty", e)
		}
	}
}
