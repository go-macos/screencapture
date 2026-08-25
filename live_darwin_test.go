// Copyright (c) the go-macos/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin && integration

// This is the live macOS proof. It runs only under -tags=integration with
// SCREENCAPTURE_INTEGRATION set, because a CI runner has neither a display nor
// the Screen Recording grant.
//
// It deliberately captures a window this very process owns, through
// SCContentFilter's desktop-independent-window path. That is not a shortcut:
// content owned by the calling process is capturable with NO TCC grant at all,
// so this proof runs on a machine where Screen Recording was never granted —
// and it still exercises the entire pipeline (SCShareableContent -> filter ->
// SCStreamConfiguration -> SCStream -> the SCStreamOutput callback ->
// CMSampleBuffer -> CVPixelBuffer -> the borrowed Go slice).
//
// What it asserts, and why each assertion would FAIL if the code were wrong:
//
//   - frames actually arrive                  (a silent no-callback wiring)
//   - the frame size is what was ASKED for    (config not reaching the stream)
//   - stride is carried, not assumed          (a sheared image)
//   - the buffer is not uniformly zero        (a stream that never painted)
//   - the CENTRE PIXEL matches the colour the window was just set to, in the
//     BGRA channel order this package promises, and CHANGES when the window's
//     colour changes                          (the classic all-grey failure,
//     and a swapped R/B)
//
// All AppKit work runs on the process MAIN OS thread, which TestMain reserves
// and pumps; the test bodies run on a test goroutine and hop over with onMain.
package screencapture

import (
	"context"
	"errors"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-macos/objc"
)

const integrationEnv = "SCREENCAPTURE_INTEGRATION"

// captureDir is where a capture of a REAL screen may be written, and it is
// never inside a repository.
//
// A screen capture is a picture of whoever ran the test, at work. This
// repository is public, and a .gitignore is a safety net rather than a barrier:
// `git add -f`, a fresh clone, or any tool that does not consult it will publish
// the file anyway. So captures do not go where they could be committed AT ALL.
//
// Set SCREENCAPTURE_ARTIFACT_DIR to keep them; otherwise they land in the
// test's own temporary directory and vanish with it. Either way the path is
// logged, so a person can go and look.
func captureDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("SCREENCAPTURE_ARTIFACT_DIR")
	if dir == "" {
		return t.TempDir()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("SCREENCAPTURE_ARTIFACT_DIR=%q: %v", dir, err)
	}
	// The refusal is the point. A directory a person chose is still checked,
	// because the mistake this prevents is exactly the one a person makes.
	if root := repoRootOf(abs); root != "" {
		t.Fatalf("SCREENCAPTURE_ARTIFACT_DIR=%q is inside the git work tree at %s; "+
			"a screen capture must never be written where it can be committed", abs, root)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatalf("SCREENCAPTURE_ARTIFACT_DIR=%q: %v", abs, err)
	}
	return abs
}

// repoRootOf returns the work tree dir contains, or "" if it is in none.
func repoRootOf(dir string) string {
	for d := dir; ; {
		if fi, err := os.Stat(filepath.Join(d, ".git")); err == nil && (fi.IsDir() || fi.Mode().IsRegular()) {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// mainfuncs carries work to the reserved main OS thread.
var mainfuncs = make(chan func(), 8)

func TestMain(m *testing.M) {
	if os.Getenv(integrationEnv) == "" {
		fmt.Fprintf(os.Stderr, "%s not set; skipping the live capture suite\n", integrationEnv)
		os.Exit(0)
	}
	runtime.LockOSThread()
	if err := objc.Load(objc.Foundation, objc.AppKit); err != nil {
		fmt.Fprintln(os.Stderr, "load AppKit:", err)
		os.Exit(1)
	}
	go func() { os.Exit(m.Run()) }()

	// The main thread both services onMain work and turns the AppKit run loop,
	// which is what actually makes an NSWindow redraw. The capture callbacks
	// themselves need no run loop — they arrive on the stream's dispatch queue
	// — but the WINDOW does, and without redraws there is nothing to capture.
	rl := objc.ClassID("NSRunLoop").Send(objc.Sel("currentRunLoop"))
	for {
		select {
		case f := <-mainfuncs:
			f()
		default:
		}
		date := objc.ClassID("NSDate").Send(objc.Sel("dateWithTimeIntervalSinceNow:"), 0.004)
		rl.Send(objc.Sel("runUntilDate:"), date)
	}
}

// onMain runs f on the reserved main OS thread and waits for it.
func onMain(f func()) {
	done := make(chan struct{})
	mainfuncs <- func() { f(); close(done) }
	<-done
}

// cgRectTest mirrors CGRect for the window geometry passed by value.
type cgRectTest struct{ X, Y, W, H float64 }

// probeWindow is a real NSWindow this process owns, used as the capture
// source. Its background colour is the signal the assertions read back.
type probeWindow struct {
	win objc.ID
	id  uint32
	w   int
	h   int
}

// newProbeWindow opens a borderless-titled window of the given size.
func newProbeWindow(t *testing.T, w, h int) *probeWindow {
	t.Helper()
	p := &probeWindow{w: w, h: h}
	onMain(func() {
		app := objc.ClassID("NSApplication").Send(objc.Sel("sharedApplication"))
		app.Send(objc.Sel("setActivationPolicy:"), 0)
		win := objc.ClassID("NSWindow").Send(objc.Sel("alloc"))
		// styleMask 1|2|8 = titled | closable | miniaturizable, backing 2 =
		// NSBackingStoreBuffered.
		win = objc.Send[objc.ID](win, objc.Sel("initWithContentRect:styleMask:backing:defer:"),
			cgRectTest{120, 120, float64(w), float64(h)}, uint64(1|2|8), uint64(2), false)
		win.Send(objc.Sel("setTitle:"), objc.NSString("go-macos/screencapture live proof"))
		win.Send(objc.Sel("makeKeyAndOrderFront:"), objc.ID(0))
		app.Send(objc.Sel("activateIgnoringOtherApps:"), true)
		p.win = win
		p.id = uint32(objc.Send[int](win, objc.Sel("windowNumber")))
	})
	if p.id == 0 {
		t.Fatal("the probe window has no window number")
	}
	t.Cleanup(func() { onMain(func() { p.win.Send(objc.Sel("close")) }) })
	// Give AppKit a beat to place and paint the window before capturing it.
	time.Sleep(300 * time.Millisecond)
	return p
}

// setColor paints the window with a named NSColor and forces a redraw.
func (p *probeWindow) setColor(name string) {
	onMain(func() {
		c := objc.ClassID("NSColor").Send(objc.Sel(name))
		p.win.Send(objc.Sel("setBackgroundColor:"), c)
		p.win.Send(objc.Sel("display"))
	})
}

// centre returns the BGRA quadruple at the middle of the frame, read through
// Frame.Row so the stride handling is what is under test.
func centre(f Frame) (b, g, r, a byte) {
	row := f.Row(f.Height / 2)
	x := (f.Width / 2) * 4
	return row[x], row[x+1], row[x+2], row[x+3]
}

// waitForColor pulls frames until the centre pixel satisfies want, or the
// deadline passes. It returns the frame that satisfied it.
func waitForColor(t *testing.T, s *Stream, want func(b, g, r byte) bool, d time.Duration) Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var last Frame
	for {
		f, err := s.WaitFrame(ctx)
		if err != nil {
			if last.Valid() {
				b, g, r, _ := centre(last)
				t.Fatalf("no frame matched within %v; last centre pixel was B=%d G=%d R=%d: %v",
					d, b, g, r, err)
			}
			t.Fatalf("no frame at all within %v: %v", d, err)
		}
		last = f
		b, g, r, _ := centre(f)
		if want(b, g, r) {
			return f
		}
	}
}

// openWindowStream finds the probe window through the permission-free
// enumeration and opens a stream on it.
func openWindowStream(t *testing.T, p *probeWindow, opt Options) *Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	content, err := CurrentProcessShareable(ctx)
	if err != nil {
		t.Fatalf("CurrentProcessShareable: %v", err)
	}
	win, err := content.Window(p.id)
	if err != nil {
		t.Fatalf("the probe window %d is not in the current-process content (%d windows): %v",
			p.id, len(content.Windows), err)
	}
	s, err := CaptureWindow(ctx, win, opt)
	if err != nil {
		t.Fatalf("CaptureWindow(%d): %v", p.id, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		// Close must be idempotent.
		if err := s.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
	})
	return s
}

// TestLiveEnumeration checks the enumeration against what the system itself
// says, through a completely different API: NSScreen.
func TestLiveEnumeration(t *testing.T) {
	if !Available() {
		t.Fatal("ScreenCaptureKit reports unavailable on a macOS that has it")
	}
	t.Logf("Authorized() = %v", Authorized())

	// NSScreen is the independent witness. It needs no permission.
	type nsScreen struct {
		id    uint32
		frame Rect
		scale float64
	}
	var want []nsScreen
	onMain(func() {
		objc.AutoreleasePool(func() {
			screens := objc.ClassID("NSScreen").Send(objc.Sel("screens"))
			for i, n := 0, int(screens.Send(objc.Sel("count"))); i < n; i++ {
				sc := screens.Send(objc.Sel("objectAtIndex:"), i)
				fr := objc.Send[cgRectTest](sc, objc.Sel("frame"))
				desc := sc.Send(objc.Sel("deviceDescription"))
				num := desc.Send(objc.Sel("objectForKey:"), objc.NSString("NSScreenNumber"))
				want = append(want, nsScreen{
					id:    objc.Send[uint32](num, objc.Sel("unsignedIntValue")),
					frame: Rect{fr.X, fr.Y, fr.W, fr.H},
					scale: objc.Send[float64](sc, objc.Sel("backingScaleFactor")),
				})
			}
		})
	})
	t.Logf("NSScreen says %d screen(s): %+v", len(want), want)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	content, err := Shareable(ctx)
	if err != nil {
		// Without the grant the display list is simply not knowable. Say so
		// loudly rather than passing quietly.
		t.Skipf("Shareable needs the Screen Recording grant and this machine has not given it: %v", err)
	}
	if len(content.Displays) != len(want) {
		t.Errorf("display count = %d, NSScreen says %d", len(content.Displays), len(want))
	}
	for _, w := range want {
		d, err := content.Display(w.id)
		if err != nil {
			t.Errorf("NSScreen lists display %d, ScreenCaptureKit does not: %v", w.id, err)
			continue
		}
		if d.Frame != w.frame {
			t.Errorf("display %d frame = %s, NSScreen says %s", w.id, d.Frame, w.frame)
		}
		if got := d.Scale(); got != w.scale {
			t.Errorf("display %d scale = %g, NSScreen says %g", w.id, got, w.scale)
		}
		t.Logf("display %d agrees with NSScreen: %s", w.id, d)
	}
}

// TestLiveCurrentProcessEnumeration proves the permission-free enumeration
// sees a window this process just opened.
func TestLiveCurrentProcessEnumeration(t *testing.T) {
	p := newProbeWindow(t, 400, 300)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	content, err := CurrentProcessShareable(ctx)
	if err != nil {
		t.Fatalf("CurrentProcessShareable: %v", err)
	}
	w, err := content.Window(p.id)
	if err != nil {
		t.Fatalf("window %d missing from %d current-process windows: %v", p.id, len(content.Windows), err)
	}
	if w.PID != 0 && int(w.PID) != os.Getpid() {
		t.Errorf("window %d is owned by pid %d, this process is %d", p.id, w.PID, os.Getpid())
	}
	if w.Frame.W != 400 {
		t.Errorf("window frame width = %g, the window was made 400 points wide", w.Frame.W)
	}
	t.Logf("found our own window: %s", w)
}

// TestLiveWindowCapture is the core proof.
func TestLiveWindowCapture(t *testing.T) {
	p := newProbeWindow(t, 400, 300)
	p.setColor("redColor")

	const reqW, reqH = 400, 300
	s := openWindowStream(t, p, Options{Width: reqW, Height: reqH, FPS: 60, QueueDepth: 6})

	// 1. A frame with the colour we just painted. Red in BGRA is B low, R high.
	f := waitForColor(t, s, func(b, g, r byte) bool { return r > 180 && b < 80 }, 8*time.Second)
	t.Logf("first matching frame: %dx%d stride=%d (Width*4=%d, padding=%d bytes/row) seq=%d",
		f.Width, f.Height, f.Stride, f.Width*4, f.Stride-f.Width*4, f.Seq)

	// 2. The size is the size we asked for.
	if f.Width != reqW || f.Height != reqH {
		t.Errorf("frame is %dx%d, %dx%d was requested", f.Width, f.Height, reqW, reqH)
	}
	// 3. Stride is carried, never assumed.
	if f.Stride < f.Width*4 {
		t.Fatalf("stride %d is below Width*4 = %d", f.Stride, f.Width*4)
	}
	if len(f.Pix) != f.Stride*f.Height {
		t.Errorf("len(Pix) = %d, stride*height = %d", len(f.Pix), f.Stride*f.Height)
	}
	// 4. The buffer is not uniformly zero, and not uniformly anything.
	var distinct = map[byte]bool{}
	for y := 0; y < f.Height; y += 7 {
		row := f.Row(y)
		for x := 0; x < len(row); x += 13 {
			distinct[row[x]] = true
		}
	}
	if len(distinct) < 3 {
		t.Errorf("the frame carries only %d distinct byte values; a uniform buffer means nothing was captured", len(distinct))
	}
	b, g, r, a := centre(f)
	t.Logf("centre pixel while red: B=%d G=%d R=%d A=%d", b, g, r, a)

	// 5. The content CHANGES. Repaint blue and demand a frame that shows it.
	p.setColor("blueColor")
	f2 := waitForColor(t, s, func(b, g, r byte) bool { return b > 180 && r < 80 }, 8*time.Second)
	b2, g2, r2, _ := centre(f2)
	t.Logf("centre pixel while blue: B=%d G=%d R=%d (was B=%d G=%d R=%d)", b2, g2, r2, b, g, r)
	if f2.Seq <= f.Seq {
		t.Errorf("the blue frame has sequence %d, not newer than the red frame's %d", f2.Seq, f.Seq)
	}

	// 6. Frame() reports freshness honestly: immediately after consuming a
	//    frame, there is nothing newer.
	if _, fresh := s.Frame(); fresh {
		// A frame may legitimately have landed in the microseconds since, so
		// only a SECOND consecutive fresh answer with no repaint is suspect.
		if _, fresh2 := s.Frame(); fresh2 {
			if _, fresh3 := s.Frame(); fresh3 {
				t.Error("Frame reported three consecutive fresh frames with nothing repainting")
			}
		}
	}

	st := s.Stats()
	t.Logf("stats: frames=%d idle=%d superseded=%d interval=%v (%.1f fps)",
		st.Frames, st.Idle, st.Superseded, st.Interval, st.FPS())
	if st.Frames < 2 {
		t.Errorf("only %d frames were delivered", st.Frames)
	}

	// 7. The PNG artefact, so a human can look at it — written outside any
	// repository, like every capture this package makes.
	dir := captureDir(t)
	img, err := f2.NRGBA()
	if err != nil {
		t.Fatalf("NRGBA: %v", err)
	}
	path := filepath.Join(dir, "window-capture.png")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(out, img); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(path)
	t.Logf("PNG artefact written to %s", abs)
}

// TestLiveFrameRate measures what the capture actually costs the consumer, on
// a window that is repainted as fast as the run loop allows. The consumer's
// budget is 16.6 ms per frame INCLUDING its own work, so anything the capture
// spends here matters.
func TestLiveFrameRate(t *testing.T) {
	p := newProbeWindow(t, 1280, 720)
	s := openWindowStream(t, p, Options{Width: 1280, Height: 720, FPS: 120, QueueDepth: 8})

	// Repaint continuously from the main thread for the measurement window.
	stop := make(chan struct{})
	go func() {
		colors := []string{"redColor", "greenColor", "blueColor", "yellowColor"}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			p.setColor(colors[i%len(colors)])
		}
	}()
	defer close(stop)

	// Warm up, then measure.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 0; i < 5; i++ {
		if _, err := s.WaitFrame(ctx); err != nil {
			t.Fatalf("warm-up frame %d: %v", i, err)
		}
	}

	const want = 120
	start := time.Now()
	var got int
	var totalPull time.Duration
	deadline := time.Now().Add(5 * time.Second)
	for got < want && time.Now().Before(deadline) {
		wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
		t0 := time.Now()
		f, err := s.WaitFrame(wctx)
		totalPull += time.Since(t0)
		wcancel()
		if err != nil {
			t.Fatalf("frame %d: %v", got, err)
		}
		if !f.Valid() {
			t.Fatalf("frame %d is not valid: %dx%d stride=%d len=%d", got, f.Width, f.Height, f.Stride, len(f.Pix))
		}
		got++
	}
	elapsed := time.Since(start)
	st := s.Stats()
	// totalPull is time spent WAITING for the window server, i.e. the frame
	// PERIOD, not what the capture costs the consumer's CPU. The cost is the
	// AllocsPerRun / benchmark figures below.
	t.Logf("MEASURED: %d frames of %dx%d in %v = %.1f fps; mean inter-frame wait %.3f ms",
		got, s.Options().Width, s.Options().Height, elapsed,
		float64(got)/elapsed.Seconds(), float64(totalPull.Microseconds())/float64(got)/1000)
	t.Logf("MEASURED: stream stats frames=%d idle=%d superseded=%d instantaneous %.1f fps",
		st.Frames, st.Idle, st.Superseded, st.FPS())

	// The cost of the ACCESS itself, with a frame already in hand: this is
	// what a compositor pays per screen per frame.
	allocs := testing.AllocsPerRun(2000, func() { s.Frame() })
	t.Logf("MEASURED: Stream.Frame allocations per call = %.2f", allocs)
	if allocs != 0 {
		t.Errorf("Stream.Frame allocates %.2f times per call; the hot path must allocate nothing", allocs)
	}

	// And the cost of a full tight copy out, which a consumer that cannot use
	// a padded buffer would pay.
	f, _ := s.Frame()
	if f.Valid() {
		dst := make([]byte, f.TightLen())
		n := 200
		t0 := time.Now()
		for i := 0; i < n; i++ {
			if _, err := f.CopyTight(dst); err != nil {
				t.Fatal(err)
			}
		}
		per := time.Since(t0) / time.Duration(n)
		t.Logf("MEASURED: CopyTight of %dx%d (%d bytes) = %.3f ms per copy",
			f.Width, f.Height, len(dst), float64(per.Microseconds())/1000)
	}

	// The consumer composites a PANORAMA, so measure the delivery pipeline at
	// that shape too. The source window is smaller; ScreenCaptureKit scales it
	// up, which is exactly the work a real 4K-wide display capture would make
	// it do.
	t.Run("panorama", func(t *testing.T) {
		s := openWindowStream(t, p, Options{Width: 3840, Height: 1080, FPS: 120, QueueDepth: 8, ScalesToFit: true})
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		for i := 0; i < 3; i++ {
			if _, err := s.WaitFrame(ctx); err != nil {
				t.Fatalf("panorama warm-up frame %d: %v", i, err)
			}
		}
		start := time.Now()
		n := 0
		for deadline := time.Now().Add(3 * time.Second); n < 60 && time.Now().Before(deadline); {
			wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
			f, err := s.WaitFrame(wctx)
			wcancel()
			if err != nil {
				break
			}
			if f.Width != 3840 || f.Height != 1080 {
				t.Fatalf("panorama frame is %dx%d, 3840x1080 was requested", f.Width, f.Height)
			}
			n++
		}
		el := time.Since(start)
		f, _ := s.Frame()
		t.Logf("MEASURED: panorama %d frames of %dx%d (stride %d, %.1f MiB each) in %v = %.1f fps",
			n, f.Width, f.Height, f.Stride, float64(len(f.Pix))/(1<<20), el, float64(n)/el.Seconds())
		allocs := testing.AllocsPerRun(2000, func() { s.Frame() })
		t.Logf("MEASURED: panorama Stream.Frame allocations per call = %.2f", allocs)
		if n < 2 {
			t.Errorf("only %d panorama frames arrived", n)
		}
	})
}

// BenchmarkFrame measures the borrowed-frame handover with -benchmem.
func BenchmarkFrame(b *testing.B) {
	t := &testing.T{}
	p := newProbeWindow(t, 640, 480)
	if t.Failed() {
		b.Fatal("could not open the probe window")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	content, err := CurrentProcessShareable(ctx)
	if err != nil {
		b.Fatal(err)
	}
	win, err := content.Window(p.id)
	if err != nil {
		b.Fatal(err)
	}
	s, err := CaptureWindow(ctx, win, Options{Width: 640, Height: 480, FPS: 60})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	if _, err := s.WaitFrame(ctx); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, _ := s.Frame()
		_ = f.Stride
	}
}

// BenchmarkCopyTight measures the copy a consumer pays only if it cannot work
// with a padded buffer.
func BenchmarkCopyTight(b *testing.B) {
	f := Frame{Width: 3840, Height: 1080, Stride: 3840*4 + 64}
	f.Pix = make([]byte, f.Stride*f.Height)
	dst := make([]byte, f.TightLen())
	b.SetBytes(int64(len(dst)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.CopyTight(dst); err != nil {
			b.Fatal(err)
		}
	}
}

// TestLiveDisplayFilterPath exercises the DISPLAY branch — SCContentFilter's
// -initWithDisplay:excludingWindows: and the exclusion list — which the public
// CaptureDisplay reaches only with the Screen Recording grant.
//
// It is a white-box test on purpose. Without the grant the public entry point
// cannot get past enumeration, and the display filter would otherwise ship
// entirely unexercised. Here the display comes from the permission-free
// current-process listing, so at least the filter construction, the exclusion
// array, the stream configuration and the start handshake are proven; whether
// the SYSTEM then hands over another application's pixels is a TCC decision,
// not a decision this code makes.
func TestLiveDisplayFilterPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// The probe window must exist BEFORE the content is listed. Shareable
	// content is a SNAPSHOT: a window created after it was taken is simply not
	// in it, so the exclusion list built from that listing comes back empty and
	// the excludingWindows: argument is never really exercised.
	p := newProbeWindow(t, 300, 200)

	// Prefer the permissioned listing when the machine has the grant, since
	// that is the real path; fall back to the permission-free one.
	raw, err := fetchContent(ctx, false)
	granted := err == nil
	if !granted {
		t.Logf("no Screen Recording grant (%v); falling back to the current-process display list", err)
		raw, err = fetchContent(ctx, true)
		if err != nil {
			t.Fatalf("even the permission-free enumeration failed: %v", err)
		}
	}
	defer raw.Send(objc.Sel("release"))

	var content *Content
	withPool(func() { content = convertContent(raw) })
	if len(content.Displays) == 0 {
		t.Skip("no display in the content listing")
	}
	d, err := content.MainDisplay()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("display under test: %s", d)

	var filter objc.ID
	var resolved Options
	var buildErr error
	opt := Options{Width: 1280, Height: 720, FPS: 30, QueueDepth: 6,
		ScalesToFit: true, ExcludeWindows: []uint32{p.id}}
	withPool(func() {
		scDisplay, excluded := findDisplay(raw, d.ID, opt.ExcludeWindows)
		if scDisplay == 0 {
			buildErr = fmt.Errorf("display %d not found in the raw content", d.ID)
			return
		}
		if n := int(excluded.Send(objc.Sel("count"))); n != 1 && granted {
			// The exclusion list can only be built from windows the listing
			// contains, so demand it only where the listing is complete.
			buildErr = fmt.Errorf("the exclusion array holds %d windows, want 1", n)
			return
		}
		filter = objc.ClassID("SCContentFilter").Send(objc.Sel("alloc")).
			Send(objc.Sel("initWithDisplay:excludingWindows:"), scDisplay, excluded)
		if filter == 0 {
			buildErr = errors.New("initWithDisplay:excludingWindows: returned nil")
			return
		}
		resolved, buildErr = opt.resolve(d.PixelWidth, d.PixelHeight)
	})
	if buildErr != nil {
		t.Fatalf("building the display filter: %v", buildErr)
	}
	t.Logf("SCContentFilter for the display built, resolved options %dx%d @ %g fps",
		resolved.Width, resolved.Height, resolved.FPS)

	s, err := newStream(ctx, filter, resolved, fmt.Sprintf("display %d", d.ID))
	if err != nil {
		if !granted {
			// This is the expected outcome without the grant, and it must be
			// the NAMED error, not a nil dereference.
			if !errors.Is(err, ErrPermissionDenied) {
				t.Errorf("starting a display stream without the grant = %v, want ErrPermissionDenied", err)
			}
			t.Skipf("display capture needs the Screen Recording grant: %v", err)
		}
		t.Fatalf("newStream on a display: %v", err)
	}
	defer s.Close()
	t.Log("the display stream STARTED")

	f, err := s.WaitFrame(ctx)
	if err != nil {
		t.Fatalf("no display frame: %v", err)
	}
	t.Logf("display frame: %dx%d stride=%d (Width*4=%d)", f.Width, f.Height, f.Stride, f.Width*4)
	if f.Width != resolved.Width || f.Height != resolved.Height {
		t.Errorf("display frame is %dx%d, %dx%d was requested",
			f.Width, f.Height, resolved.Width, resolved.Height)
	}
	dir := captureDir(t)
	img, err := f.NRGBA()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "display-capture.png")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(out, img); err != nil {
		t.Fatal(err)
	}
	out.Close()
	abs, _ := filepath.Abs(path)
	t.Logf("display PNG artefact written to %s", abs)
}
