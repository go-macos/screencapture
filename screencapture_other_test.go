// Copyright (c) the go-macos/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !darwin

// The non-darwin lane. Every entry point must answer ErrUnsupported rather than
// failing to build or panicking, because a consumer cross-compiles this package
// without thinking about it.
package screencapture

import (
	"context"
	"errors"
	"testing"
)

func TestOtherEntryPointsAreUnsupported(t *testing.T) {
	ctx := context.Background()
	if Available() {
		t.Error("Available() must be false where ScreenCaptureKit does not exist")
	}
	if Authorized() {
		t.Error("Authorized() must be false where there is no grant to hold")
	}
	if RequestAuthorization() {
		t.Error("RequestAuthorization() must be false and must prompt nothing")
	}
	if _, err := Shareable(ctx); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Shareable = %v, want ErrUnsupported", err)
	}
	if _, err := CurrentProcessShareable(ctx); !errors.Is(err, ErrUnsupported) {
		t.Errorf("CurrentProcessShareable = %v, want ErrUnsupported", err)
	}
	if _, err := Displays(ctx); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Displays = %v, want ErrUnsupported", err)
	}
	if _, err := Windows(ctx); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Windows = %v, want ErrUnsupported", err)
	}
	if _, err := CaptureDisplay(ctx, Display{ID: 1}, Options{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("CaptureDisplay = %v, want ErrUnsupported", err)
	}
	if _, err := CaptureWindow(ctx, Window{ID: 1}, Options{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("CaptureWindow = %v, want ErrUnsupported", err)
	}
}

// TestOtherCaptureValidatesFirst proves a consumer's option bug reports the
// SAME error on every platform, rather than being masked by ErrUnsupported.
func TestOtherCaptureValidatesFirst(t *testing.T) {
	ctx := context.Background()
	bad := Options{Width: 100} // height missing
	if _, err := CaptureDisplay(ctx, Display{}, bad); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("CaptureDisplay with bad options = %v, want ErrInvalidOption", err)
	}
	if _, err := CaptureWindow(ctx, Window{}, bad); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("CaptureWindow with bad options = %v, want ErrInvalidOption", err)
	}
}

func TestOtherStreamMethods(t *testing.T) {
	s := &Stream{opt: Options{Width: 7}, source: "nowhere"}
	if s.Options().Width != 7 || s.Source() != "nowhere" {
		t.Errorf("Options/Source = %+v %q", s.Options(), s.Source())
	}
	if f, fresh := s.Frame(); fresh || f.Valid() {
		t.Errorf("Frame() = %+v, fresh=%v", f, fresh)
	}
	if _, err := s.WaitFrame(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("WaitFrame = %v, want ErrUnsupported", err)
	}
	if st := s.Stats(); st != (Stats{}) {
		t.Errorf("Stats() = %+v, want the zero value", st)
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close() must be idempotent, second call = %v", err)
	}
}
