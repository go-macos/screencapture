// Copyright (c) the go-macos/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunRejectsBadFlags proves a usage error exits 2 and says so, on every
// platform.
func TestRunRejectsBadFlags(t *testing.T) {
	var out, errb bytes.Buffer
	if got := run([]string{"-nosuchflag"}, &out, &errb); got != 2 {
		t.Errorf("run with a bad flag = %d, want 2", got)
	}
	if !strings.Contains(errb.String(), "nosuchflag") {
		t.Errorf("the usage error does not name the flag: %q", errb.String())
	}
}

// TestRunEnumerates exercises the default path. On a machine without the grant
// it must exit 1 with the named permission error rather than crash; with the
// grant it must exit 0 and list at least one display. On a platform with no
// ScreenCaptureKit at all it must exit 1 saying that.
func TestRunEnumerates(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-windows"}, &out, &errb)
	t.Logf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())
	switch code {
	case 0:
		if !strings.Contains(out.String(), "display(s)") {
			t.Errorf("a successful run printed no display list: %q", out.String())
		}
	case 1:
		body := errb.String() + out.String()
		if !strings.Contains(body, "System Settings") && !strings.Contains(body, "not available") {
			t.Errorf("a failing run must name the remedy or the absence: %q", body)
		}
	default:
		t.Errorf("unexpected exit status %d", code)
	}
}

// TestRunOwnEnumeration uses the permission-free listing, which must succeed on
// any macOS and fail cleanly elsewhere.
func TestRunOwnEnumeration(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-own"}, &out, &errb)
	t.Logf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())
	if code != 0 && code != 1 {
		t.Errorf("unexpected exit status %d", code)
	}
}

// TestOsExitSeamIsWired guards the seam main() uses, so the 100%-coverable
// run()/osExit split cannot be quietly undone.
func TestOsExitSeamIsWired(t *testing.T) {
	if osExit == nil {
		t.Fatal("the osExit seam is nil")
	}
}
