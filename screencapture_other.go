// Copyright (c) the go-macos/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !darwin

package screencapture

import "context"

// On every platform that is not macOS there is no ScreenCaptureKit, so the
// entry points below report [ErrUnsupported] rather than failing to build. A
// consumer cross-compiles this package without having to think about it, and
// gets one clear error at run time instead of a link failure at build time.
//
// Everything portable — the option validation, the stride arithmetic, the
// BGRA-to-RGBA conversion, the error mapping — lives in screencapture.go and
// is fully exercised here, which is what lets the Linux lane in CI cover it.

// Available reports false: ScreenCaptureKit exists only on macOS.
func Available() bool { return false }

// Authorized reports false: there is no Screen Recording grant to hold.
func Authorized() bool { return false }

// RequestAuthorization reports false and prompts nothing.
func RequestAuthorization() bool { return false }

// Shareable reports [ErrUnsupported].
func Shareable(ctx context.Context) (*Content, error) { return nil, ErrUnsupported }

// CurrentProcessShareable reports [ErrUnsupported].
func CurrentProcessShareable(ctx context.Context) (*Content, error) { return nil, ErrUnsupported }

// Displays reports [ErrUnsupported].
func Displays(ctx context.Context) ([]Display, error) { return nil, ErrUnsupported }

// Windows reports [ErrUnsupported].
func Windows(ctx context.Context) ([]Window, error) { return nil, ErrUnsupported }

// CaptureDisplay reports [ErrUnsupported]. It still validates the options
// first, so a consumer's option bug surfaces identically on every platform.
func CaptureDisplay(ctx context.Context, d Display, opt Options) (*Stream, error) {
	if err := opt.Validate(); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}

// CaptureWindow reports [ErrUnsupported], after the same option validation as
// [CaptureDisplay].
func CaptureWindow(ctx context.Context, w Window, opt Options) (*Stream, error) {
	if err := opt.Validate(); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}

// Stream is the non-darwin stand-in for a live capture. It can never be
// created here — [CaptureDisplay] and [CaptureWindow] always fail — but the
// type and its methods exist so consumer code compiles unchanged.
type Stream struct {
	opt    Options
	source string
}

// Options returns the stream's resolved options.
func (s *Stream) Options() Options { return s.opt }

// Source names what is being captured.
func (s *Stream) Source() string { return s.source }

// Frame reports the zero frame and false.
func (s *Stream) Frame() (Frame, bool) { return Frame{}, false }

// WaitFrame reports [ErrUnsupported].
func (s *Stream) WaitFrame(ctx context.Context) (Frame, error) { return Frame{}, ErrUnsupported }

// Stats reports the zero statistics.
func (s *Stream) Stats() Stats { return Stats{} }

// Err reports nil.
func (s *Stream) Err() error { return nil }

// Close reports nil and is idempotent.
func (s *Stream) Close() error { return nil }
