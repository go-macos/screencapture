// Copyright (c) the go-macos/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

// Command sccheck reports what github.com/go-macos/screencapture can see and
// capture on this machine, and is the quickest way to find out whether the
// Screen Recording permission is in place.
//
//	sccheck                          # permission, displays, window count
//	sccheck -windows                 # the full window list
//	sccheck -request                 # ask the system for the permission
//	sccheck -display 1 -n 30 -o d.png
//	sccheck -window 1234 -n 30 -o w.png
//
// A capture run reports the frames it actually received, the rate it achieved
// and the milliseconds per frame, and writes the last frame to a PNG.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image/png"
	"io"
	"os"
	"time"

	"github.com/go-macos/screencapture"
)

// osExit is a seam so run's exit paths stay testable.
var osExit = os.Exit

func main() { osExit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run is the whole command. It returns the process exit status: 0 for success,
// 1 for a failure, 2 for a usage error.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sccheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		listWindows = fs.Bool("windows", false, "list every capturable window")
		request     = fs.Bool("request", false, "ask the system for the Screen Recording permission")
		displayID   = fs.Uint("display", 0, "capture this CGDirectDisplayID (0 = the main display)")
		windowID    = fs.Uint("window", 0, "capture this CGWindowID instead of a display")
		own         = fs.Bool("own", false, "enumerate only this process's own content (needs no permission)")
		frames      = fs.Int("n", 0, "capture this many frames, then stop")
		out         = fs.String("o", "", "write the last captured frame to this PNG")
		width       = fs.Int("w", 0, "capture width in pixels (0 = native)")
		height      = fs.Int("h", 0, "capture height in pixels (0 = native)")
		fps         = fs.Float64("fps", 60, "frame-rate ceiling")
		cursor      = fs.Bool("cursor", false, "draw the mouse pointer into the capture")
		timeout     = fs.Duration("timeout", 30*time.Second, "give up after this long")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if !screencapture.Available() {
		fmt.Fprintln(stderr, "ScreenCaptureKit is not available on this system")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if *request {
		fmt.Fprintf(stdout, "requesting Screen Recording permission…\n")
		granted := screencapture.RequestAuthorization()
		fmt.Fprintf(stdout, "granted: %v\n", granted)
		if !granted {
			fmt.Fprintf(stderr, "\n%v\n", screencapture.ErrPermissionDenied)
			fmt.Fprintln(stderr, "\nNote: once an application has been refused, the system records the")
			fmt.Fprintln(stderr, "refusal and shows NO further prompt. Only System Settings can change it.")
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "ScreenCaptureKit: available\n")
	fmt.Fprintf(stdout, "Screen Recording permission: %v\n", screencapture.Authorized())

	if *frames > 0 {
		return capture(ctx, stdout, stderr, *displayID, *windowID, *own, *frames, *out,
			screencapture.Options{
				Width: *width, Height: *height, FPS: *fps, ShowsCursor: *cursor,
			})
	}

	content, err := enumerate(ctx, *own)
	if err != nil {
		fmt.Fprintf(stderr, "\n%v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "\n%d display(s):\n", len(content.Displays))
	for _, d := range content.Displays {
		main := ""
		if d.Main {
			main = "  [main]"
		}
		fmt.Fprintf(stdout, "  %s  scale %g%s\n", d, d.Scale(), main)
	}
	fmt.Fprintf(stdout, "\n%d window(s), %d application(s)\n",
		len(content.Windows), len(content.Applications))
	if *listWindows {
		for _, w := range content.Windows {
			onscreen := ""
			if w.OnScreen {
				onscreen = " on-screen"
			}
			fmt.Fprintf(stdout, "  %s layer %d%s\n", w, w.Layer, onscreen)
		}
	} else if len(content.Windows) > 0 {
		fmt.Fprintln(stdout, "  (pass -windows for the list)")
	}
	return 0
}

// enumerate picks the permissioned or the permission-free listing.
func enumerate(ctx context.Context, own bool) (*screencapture.Content, error) {
	if own {
		return screencapture.CurrentProcessShareable(ctx)
	}
	return screencapture.Shareable(ctx)
}

// capture opens a stream, pulls n frames, reports the rate and writes a PNG.
func capture(ctx context.Context, stdout, stderr io.Writer, displayID, windowID uint,
	own bool, n int, out string, opt screencapture.Options) int {
	content, err := enumerate(ctx, own || windowID != 0)
	if err != nil && windowID != 0 {
		// A window of our own is capturable without a grant; fall back.
		content, err = screencapture.CurrentProcessShareable(ctx)
	}
	if err != nil {
		fmt.Fprintf(stderr, "\n%v\n", err)
		return 1
	}

	var s *screencapture.Stream
	var what string
	if windowID != 0 {
		w, werr := content.Window(uint32(windowID))
		if werr != nil {
			fmt.Fprintf(stderr, "\n%v\n", werr)
			return 1
		}
		what = w.String()
		s, err = screencapture.CaptureWindow(ctx, w, opt)
	} else {
		var d screencapture.Display
		if displayID == 0 {
			d, err = content.MainDisplay()
		} else {
			d, err = content.Display(uint32(displayID))
		}
		if err != nil {
			fmt.Fprintf(stderr, "\n%v\n", err)
			return 1
		}
		what = d.String()
		s, err = screencapture.CaptureDisplay(ctx, d, opt)
	}
	if err != nil {
		fmt.Fprintf(stderr, "\n%v\n", err)
		return 1
	}
	defer s.Close()

	fmt.Fprintf(stdout, "\ncapturing %s at %dx%d, ceiling %g fps\n",
		what, s.Options().Width, s.Options().Height, s.Options().FPS)

	var last screencapture.Frame
	got := 0
	start := time.Now()
	for got < n {
		f, err := s.WaitFrame(ctx)
		if err != nil {
			if errors.Is(err, screencapture.ErrNoFrame) && got > 0 {
				// Not a failure: nothing on the captured surface moved.
				break
			}
			fmt.Fprintf(stderr, "\nframe %d: %v\n", got, err)
			return 1
		}
		last = f
		got++
	}
	elapsed := time.Since(start)
	st := s.Stats()
	fmt.Fprintf(stdout, "%d frames in %v = %.1f fps (%.3f ms per frame)\n",
		got, elapsed.Round(time.Millisecond), float64(got)/elapsed.Seconds(),
		float64(elapsed.Microseconds())/float64(max(got, 1))/1000)
	fmt.Fprintf(stdout, "delivered=%d idle=%d superseded=%d\n", st.Frames, st.Idle, st.Superseded)
	if !last.Valid() {
		fmt.Fprintln(stderr, "no valid frame was captured")
		return 1
	}
	fmt.Fprintf(stdout, "last frame: %dx%d stride=%d (Width*4=%d, %d bytes of row padding)\n",
		last.Width, last.Height, last.Stride, last.Width*4, last.Stride-last.Width*4)

	if out == "" {
		return 0
	}
	img, err := last.NRGBA()
	if err != nil {
		fmt.Fprintf(stderr, "\n%v\n", err)
		return 1
	}
	f, err := os.Create(out)
	if err != nil {
		fmt.Fprintf(stderr, "\n%v\n", err)
		return 1
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		fmt.Fprintf(stderr, "\n%v\n", err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(stderr, "\n%v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s\n", out)
	return 0
}
