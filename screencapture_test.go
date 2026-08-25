// Copyright (c) the go-macos/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

// Tests for the portable layer. They build and run on EVERY platform — that is
// the point of keeping the stride arithmetic, the option validation, the
// format conversion and the error mapping out of the Objective-C file. The CI
// Linux lane runs exactly this file, and gates screencapture.go at 100%
// including its error branches.
package screencapture

import (
	"errors"
	"fmt"
	"image"
	"strings"
	"testing"
	"time"
)

func TestPixelFormatString(t *testing.T) {
	if got := FormatBGRA.String(); got != "BGRA" {
		t.Errorf("FormatBGRA.String() = %q, want %q", got, "BGRA")
	}
	// A format whose bytes are not printable ASCII must not produce mojibake.
	odd := PixelFormat(0x00010203)
	got := odd.String()
	if !strings.HasPrefix(got, "PixelFormat(") {
		t.Errorf("PixelFormat(%#x).String() = %q, want the numeric form", uint32(odd), got)
	}
	if got := PixelFormat(0x7f424752).String(); !strings.HasPrefix(got, "PixelFormat(") {
		t.Errorf("a high byte of 0x7f is printable, 0x7f itself is DEL and is not: got %q", got)
	}
}

func TestPixelFormatBytesPerPixel(t *testing.T) {
	if got := FormatBGRA.BytesPerPixel(); got != 4 {
		t.Errorf("FormatBGRA.BytesPerPixel() = %d, want 4", got)
	}
	if got := PixelFormat(0).BytesPerPixel(); got != 0 {
		t.Errorf("an unknown format has no pixel size, got %d", got)
	}
}

func TestRect(t *testing.T) {
	r := Rect{1, 2, 3, 4}
	if got, want := r.String(), "(1,2)+(3×4)"; got != want {
		t.Errorf("Rect.String() = %q, want %q", got, want)
	}
	if r.Empty() {
		t.Error("a 3×4 rectangle is not empty")
	}
	for _, e := range []Rect{{W: 0, H: 4}, {W: 3, H: 0}, {W: -1, H: 4}, {W: 3, H: -1}} {
		if !e.Empty() {
			t.Errorf("%s should be empty", e)
		}
	}
}

func TestDisplayScaleAndString(t *testing.T) {
	d := Display{ID: 7, Width: 1920, Height: 1080, PixelWidth: 3840, PixelHeight: 2160,
		Frame: Rect{0, 0, 1920, 1080}}
	if got := d.Scale(); got != 2 {
		t.Errorf("Scale() = %g, want 2", got)
	}
	// A display that reports no width must not divide by zero.
	if got := (Display{PixelWidth: 100}).Scale(); got != 1 {
		t.Errorf("Scale() of a zero-width display = %g, want 1", got)
	}
	if got := d.String(); !strings.Contains(got, "1920x1080 pt") || !strings.Contains(got, "3840x2160 px") {
		t.Errorf("Display.String() = %q, want both the point and the pixel size", got)
	}
}

func TestWindowString(t *testing.T) {
	w := Window{ID: 42, Title: "Hello", AppName: "Finder", Frame: Rect{1, 2, 3, 4}}
	got := w.String()
	for _, want := range []string{"42", `"Hello"`, "Finder"} {
		if !strings.Contains(got, want) {
			t.Errorf("Window.String() = %q, missing %q", got, want)
		}
	}
}

func testContent() *Content {
	return &Content{
		Displays: []Display{
			{ID: 1, Width: 100, Height: 50},
			{ID: 2, Width: 200, Height: 100, Main: true},
		},
		Windows: []Window{
			{ID: 10, Title: "a", PID: 5},
			{ID: 11, Title: "b", PID: 5},
			{ID: 12, Title: "a", PID: 6},
		},
	}
}

func TestContentLookups(t *testing.T) {
	c := testContent()

	if d, err := c.Display(2); err != nil || d.Width != 200 {
		t.Errorf("Display(2) = %v, %v", d, err)
	}
	if _, err := c.Display(99); !errors.Is(err, ErrNotFound) {
		t.Errorf("Display(99) error = %v, want ErrNotFound", err)
	}

	if d, err := c.MainDisplay(); err != nil || d.ID != 2 {
		t.Errorf("MainDisplay() = %v, %v, want the display flagged Main", d, err)
	}
	// With no display flagged main, the first one stands in.
	noMain := &Content{Displays: []Display{{ID: 3}, {ID: 4}}}
	if d, err := noMain.MainDisplay(); err != nil || d.ID != 3 {
		t.Errorf("MainDisplay() with none flagged = %v, %v, want display 3", d, err)
	}
	if _, err := (&Content{}).MainDisplay(); !errors.Is(err, ErrNoDisplay) {
		t.Errorf("MainDisplay() of an empty content should report ErrNoDisplay, got %v", err)
	}

	if w, err := c.Window(11); err != nil || w.Title != "b" {
		t.Errorf("Window(11) = %v, %v", w, err)
	}
	if _, err := c.Window(99); !errors.Is(err, ErrNotFound) {
		t.Errorf("Window(99) error = %v, want ErrNotFound", err)
	}

	if got := c.WindowsByTitle("a"); len(got) != 2 {
		t.Errorf("WindowsByTitle(\"a\") returned %d windows, want 2", len(got))
	}
	if got := c.WindowsByTitle("zzz"); got != nil {
		t.Errorf("WindowsByTitle of an absent title = %v, want nil", got)
	}
	if got := c.WindowsOfPID(5); len(got) != 2 {
		t.Errorf("WindowsOfPID(5) returned %d windows, want 2", len(got))
	}
	if got := c.WindowsOfPID(999); got != nil {
		t.Errorf("WindowsOfPID of an absent pid = %v, want nil", got)
	}
}

func TestOptionsValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  Options
		want string // substring the error must carry; "" means it must succeed
	}{
		{"zero is valid", Options{}, ""},
		{"full house", Options{Width: 100, Height: 50, FPS: 30, QueueDepth: 4}, ""},
		{"negative width", Options{Width: -1, Height: -1}, "negative size"},
		{"negative height", Options{Width: 10, Height: -1}, "negative size"},
		{"width without height", Options{Width: 100}, "both be set"},
		{"height without width", Options{Height: 100}, "both be set"},
		{"width too large", Options{Width: MaxDimension + 1, Height: 1}, "exceeds"},
		{"height too large", Options{Width: 1, Height: MaxDimension + 1}, "exceeds"},
		{"negative fps", Options{FPS: -1}, "negative FPS"},
		{"fps too small", Options{FPS: 0.001}, "below the 0.01 minimum"},
		{"negative queue depth", Options{QueueDepth: -1}, "negative QueueDepth"},
		{"queue depth too shallow", Options{QueueDepth: 2}, "below the minimum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opt.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.want)
			}
			if !errors.Is(err, ErrInvalidOption) {
				t.Errorf("Validate() error does not wrap ErrInvalidOption: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestOptionsResolve(t *testing.T) {
	// The zero options take the source's native size and the defaults.
	got, err := Options{}.resolve(1920, 1080)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Width != 1920 || got.Height != 1080 {
		t.Errorf("resolved size = %dx%d, want the native 1920x1080", got.Width, got.Height)
	}
	if got.FPS != DefaultFPS {
		t.Errorf("resolved FPS = %g, want %g", got.FPS, DefaultFPS)
	}
	if got.QueueDepth != DefaultQueueDepth {
		t.Errorf("resolved QueueDepth = %d, want %d", got.QueueDepth, DefaultQueueDepth)
	}

	// An explicit size survives untouched.
	got, err = Options{Width: 640, Height: 480, FPS: 24, QueueDepth: 3}.resolve(1920, 1080)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Width != 640 || got.Height != 480 || got.FPS != 24 || got.QueueDepth != 3 {
		t.Errorf("resolve overwrote explicit options: %+v", got)
	}

	// An invalid option is rejected before anything is filled in.
	if _, err := (Options{FPS: -5}).resolve(100, 100); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("resolve of invalid options = %v, want ErrInvalidOption", err)
	}
	// No size given AND the source cannot say how big it is.
	for _, native := range [][2]int{{0, 100}, {100, 0}, {-1, -1}} {
		if _, err := (Options{}).resolve(native[0], native[1]); !errors.Is(err, ErrInvalidOption) {
			t.Errorf("resolve with a %v source = %v, want ErrInvalidOption", native, err)
		}
	}
}

func TestFrameInterval(t *testing.T) {
	v, ts := frameInterval(60)
	if ts != frameIntervalTimescale {
		t.Errorf("timescale = %d, want %d", ts, frameIntervalTimescale)
	}
	if v != 16667 {
		t.Errorf("frameInterval(60) numerator = %d, want 16667 (1/60 s in microseconds)", v)
	}
	// The round trip must land back on the requested rate.
	if fps := float64(ts) / float64(v); fps < 59.99 || fps > 60.01 {
		t.Errorf("frameInterval(60) round-trips to %g fps", fps)
	}
	if v, _ := frameInterval(0); v != 16667 {
		t.Errorf("frameInterval(0) = %d, want the DefaultFPS interval 16667", v)
	}
	if v, _ := frameInterval(-1); v != 16667 {
		t.Errorf("frameInterval(-1) = %d, want the DefaultFPS interval 16667", v)
	}
	// A rate faster than the timescale itself clamps to one tick rather than
	// asking for a zero interval, which CoreMedia would read as "invalid".
	if v, _ := frameInterval(1e9); v != 1 {
		t.Errorf("frameInterval(1e9) = %d, want 1", v)
	}
	if v, _ := frameInterval(1); v != 1_000_000 {
		t.Errorf("frameInterval(1) = %d, want 1000000", v)
	}
}

// makeFrame builds a synthetic frame whose every pixel encodes its own
// position, so a stride mistake shows up as wrong data rather than as a crash.
// The padding bytes are filled with 0xEE — a value no correct reader may ever
// return.
func makeFrame(w, h, pad int) Frame {
	stride := w*4 + pad
	pix := make([]byte, stride*h)
	for i := range pix {
		pix[i] = 0xEE
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := y*stride + x*4
			pix[o+0] = byte(x)       // B
			pix[o+1] = byte(y)       // G
			pix[o+2] = byte(x + y)   // R
			pix[o+3] = byte(255 - y) // A
		}
	}
	return Frame{Pix: pix, Width: w, Height: h, Stride: stride, Seq: 1, At: time.Unix(0, 0)}
}

func TestFrameValid(t *testing.T) {
	good := makeFrame(4, 3, 8)
	if !good.Valid() {
		t.Fatal("a well-formed frame must be valid")
	}
	for name, f := range map[string]Frame{
		"zero":          {},
		"no width":      {Width: 0, Height: 3, Stride: 16, Pix: make([]byte, 48)},
		"no height":     {Width: 4, Height: 0, Stride: 16, Pix: make([]byte, 48)},
		"stride short":  {Width: 4, Height: 3, Stride: 15, Pix: make([]byte, 48)},
		"pix too short": {Width: 4, Height: 3, Stride: 16, Pix: make([]byte, 47)},
	} {
		if f.Valid() {
			t.Errorf("%s frame reported valid", name)
		}
	}
}

func TestFrameTightLen(t *testing.T) {
	if got := makeFrame(4, 3, 8).TightLen(); got != 48 {
		t.Errorf("TightLen() = %d, want 48", got)
	}
}

func TestFrameRow(t *testing.T) {
	f := makeFrame(4, 3, 8)
	row := f.Row(1)
	if len(row) != 16 {
		t.Fatalf("Row returned %d bytes, want Width*4 = 16", len(row))
	}
	// The padding must NOT be visible: 0xEE anywhere means the stride was
	// treated as Width*4.
	for i, b := range row {
		if b == 0xEE {
			t.Fatalf("Row(1)[%d] is padding (0xEE); the stride was ignored", i)
		}
	}
	if row[0] != 0 || row[4] != 1 || row[1] != 1 {
		t.Errorf("Row(1) = %v, want the pixels of row 1", row[:8])
	}
	// Appending to a row must not scribble into the next row: the returned
	// slice is capped.
	row = append(row, 0x11)
	if f.Pix[1*f.Stride+16] != 0xEE {
		t.Error("append to a Row overwrote the frame's padding; the slice is not capped")
	}

	for _, y := range []int{-1, 3, 100} {
		if got := f.Row(y); got != nil {
			t.Errorf("Row(%d) = %v, want nil", y, got)
		}
	}
	if got := (Frame{}).Row(0); got != nil {
		t.Errorf("Row on an invalid frame = %v, want nil", got)
	}
}

func TestFrameCopyTight(t *testing.T) {
	f := makeFrame(4, 3, 8)
	dst := make([]byte, f.TightLen())
	n, err := f.CopyTight(dst)
	if err != nil {
		t.Fatalf("CopyTight: %v", err)
	}
	if n != 48 {
		t.Errorf("CopyTight wrote %d bytes, want 48", n)
	}
	for i, b := range dst {
		if b == 0xEE {
			t.Fatalf("CopyTight copied padding at byte %d", i)
		}
	}
	for y := 0; y < f.Height; y++ {
		for x := 0; x < f.Width; x++ {
			o := (y*f.Width + x) * 4
			if dst[o] != byte(x) || dst[o+1] != byte(y) {
				t.Fatalf("pixel (%d,%d) = %v, want B=%d G=%d", x, y, dst[o:o+4], x, y)
			}
		}
	}

	// The unpadded fast path must produce the same bytes.
	tight := makeFrame(4, 3, 0)
	dst2 := make([]byte, tight.TightLen())
	if _, err := tight.CopyTight(dst2); err != nil {
		t.Fatalf("CopyTight on an unpadded frame: %v", err)
	}
	if string(dst) != string(dst2) {
		t.Error("the padded and unpadded paths produced different bytes")
	}

	if _, err := f.CopyTight(make([]byte, 47)); !errors.Is(err, ErrShortBuffer) {
		t.Errorf("CopyTight into a short buffer = %v, want ErrShortBuffer", err)
	}
	if _, err := (Frame{}).CopyTight(dst); !errors.Is(err, ErrNoFrame) {
		t.Errorf("CopyTight of an invalid frame = %v, want ErrNoFrame", err)
	}
}

func TestFrameNRGBA(t *testing.T) {
	f := makeFrame(4, 3, 8)
	img, err := f.NRGBA()
	if err != nil {
		t.Fatalf("NRGBA: %v", err)
	}
	if img.Bounds() != image.Rect(0, 0, 4, 3) {
		t.Errorf("bounds = %v, want 0,0,4,3", img.Bounds())
	}
	// The whole point: BGRA in, RGBA out. A frame pixel of B=x G=y R=x+y must
	// come out as R=x+y G=y B=x.
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			o := y*img.Stride + x*4
			if img.Pix[o+0] != byte(x+y) || img.Pix[o+1] != byte(y) ||
				img.Pix[o+2] != byte(x) || img.Pix[o+3] != byte(255-y) {
				t.Fatalf("pixel (%d,%d) = %v; want R=%d G=%d B=%d A=%d — the channel swap is wrong",
					x, y, img.Pix[o:o+4], x+y, y, x, 255-y)
			}
		}
	}
	if _, err := (Frame{}).NRGBA(); !errors.Is(err, ErrNoFrame) {
		t.Errorf("NRGBA of an invalid frame = %v, want ErrNoFrame", err)
	}
}

func TestStatsFPS(t *testing.T) {
	if got := (Stats{Interval: 20 * time.Millisecond}).FPS(); got < 49.9 || got > 50.1 {
		t.Errorf("FPS() = %g, want 50", got)
	}
	if got := (Stats{}).FPS(); got != 0 {
		t.Errorf("FPS() with no interval = %g, want 0", got)
	}
	if got := (Stats{Interval: -1}).FPS(); got != 0 {
		t.Errorf("FPS() with a negative interval = %g, want 0", got)
	}
}

func TestStreamError(t *testing.T) {
	e := newStreamError("getShareableContent", -3801, "The user declined TCCs")
	if e.Name != "SCStreamErrorUserDeclined" {
		t.Errorf("Name = %q, want SCStreamErrorUserDeclined", e.Name)
	}
	msg := e.Error()
	for _, want := range []string{"getShareableContent", "SCStreamErrorUserDeclined", "-3801",
		"The user declined TCCs", "System Settings"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, missing %q — a denial must say what to DO", msg, want)
		}
	}
	if !errors.Is(e, ErrPermissionDenied) {
		t.Error("-3801 must match ErrPermissionDenied")
	}

	// A code with a name but no sentinel.
	other := newStreamError("startCapture", -3811, "boom")
	if !strings.Contains(other.Error(), "SCStreamErrorInternalError") {
		t.Errorf("Error() = %q, want the Apple constant", other.Error())
	}
	if errors.Unwrap(other) != nil {
		t.Errorf("-3811 unwraps to %v, want nil", errors.Unwrap(other))
	}

	// An unknown code, and empty op/message: the message must still be
	// readable rather than full of holes.
	bare := &StreamError{Code: -1}
	msg = bare.Error()
	for _, want := range []string{"ScreenCaptureKit", "SCStreamError", "-1", "no message"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() of a bare StreamError = %q, missing %q", msg, want)
		}
	}

	for code, want := range map[int]error{
		-3801: ErrPermissionDenied,
		-3803: ErrPermissionDenied,
		-3813: ErrNotFound,
		-3814: ErrNoDisplay,
		-3815: ErrNoDisplay,
		-3817: nil,
	} {
		got := errors.Unwrap(newStreamError("op", code, "m"))
		if got != want {
			t.Errorf("code %d unwraps to %v, want %v", code, got, want)
		}
	}

	// Every code in the table must have a non-empty Apple constant, and every
	// name must start with SCStreamError.
	for code, name := range streamErrorNames {
		if !strings.HasPrefix(name, "SCStreamError") {
			t.Errorf("code %d maps to %q, which is not an SCStreamError constant", code, name)
		}
	}
	if len(streamErrorNames) != 21 {
		t.Errorf("the SCError.h table holds %d codes, SCError.h defines 21", len(streamErrorNames))
	}
}

// TestSentinelsAreDistinct guards against a copy-paste that makes two sentinels
// the same error value, which would silently break errors.Is for consumers.
func TestSentinelsAreDistinct(t *testing.T) {
	all := []error{ErrUnsupported, ErrPermissionDenied, ErrNoDisplay, ErrNotFound,
		ErrClosed, ErrNoFrame, ErrInvalidOption, ErrShortBuffer}
	for i, a := range all {
		if a == nil || a.Error() == "" {
			t.Fatalf("sentinel %d is empty", i)
		}
		if !strings.HasPrefix(a.Error(), "screencapture: ") {
			t.Errorf("sentinel %q does not carry the package prefix", a)
		}
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("sentinels %d and %d are the same error", i, j)
			}
		}
	}
}

// TestPermissionMessageNamesTheRemedy is the assertion that the denied case is
// a first-class feature: the message must tell a human exactly what to do.
func TestPermissionMessageNamesTheRemedy(t *testing.T) {
	msg := ErrPermissionDenied.Error()
	for _, want := range []string{"System Settings", "Privacy & Security",
		"Screen & System Audio Recording", "restart"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ErrPermissionDenied is missing %q; it reads:\n%s", want, msg)
		}
	}
}

func ExampleFrame_Row() {
	f := makeFrame(2, 2, 8)
	fmt.Println(f.Stride, len(f.Row(0)))
	// Output: 16 8
}
