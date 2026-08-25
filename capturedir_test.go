// Copyright (c) the go-macos/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

package screencapture

// Where a capture may be written, and — the part that matters — where it may
// not.
//
// This file is deliberately UNTAGGED. The live suite that takes captures is
// behind `darwin && integration` plus SCREENCAPTURE_INTEGRATION, and it needs a
// window server; a guard that only compiles there is a guard nobody runs. The
// rule it enforces is plain filesystem reasoning with nothing macOS about it,
// so it is checked on every platform, on every lane, on every push — and the
// REFUSAL is exercised, not just the acceptance, because a guard whose failing
// branch never runs is not known to work.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureDir is where a capture of a REAL screen may be written: somewhere
// durable, and never inside a repository.
//
// A screen capture is a picture of whoever ran the test, at work. This
// repository is public, and a .gitignore is a safety net rather than a barrier:
// `git add -f`, a fresh clone, or any tool that does not consult it will publish
// the file anyway. So captures do not go where they could be committed at all.
//
// Nor do they go somewhere that evaporates. The artefact exists SO THAT A PERSON
// CAN LOOK AT IT, and t.TempDir() is removed when the test ends — it would be
// gone before anyone could. The default is therefore the user's own application
// support directory, which survives the test and the machine restarting.
// SCREENCAPTURE_ARTIFACT_DIR overrides it. The path is logged either way.
func captureDir(t testing.TB) string {
	t.Helper()
	dir, err := chooseCaptureDir(os.Getenv(captureDirEnv))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("capture directory %q: %v", dir, err)
	}
	t.Logf("captures go to %s", dir)
	return dir
}

// captureDirEnv overrides where captures go. It is still checked.
const captureDirEnv = "SCREENCAPTURE_ARTIFACT_DIR"

// chooseCaptureDir is the decision, separated from the test plumbing so the
// REFUSAL can be exercised without writing a capture somewhere to find out.
//
// want is the caller's choice, or "" to use the default.
func chooseCaptureDir(want string) (string, error) {
	// chosen names the directory the way the person who has to read a failure
	// would: by the variable when they set one, by what it is otherwise.
	dir, chosen := want, captureDirEnv
	if dir == "" {
		chosen = "the default capture directory"
		base, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("no user configuration directory to keep captures in: %w", err)
		}
		dir = filepath.Join(base, "go-macos-screencapture", "captures")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%s (%q): %w", chosen, dir, err)
	}
	// The refusal is the point. A directory a person chose is still checked,
	// because the mistake this prevents is exactly the one a person makes.
	if root := repoRootOf(abs); root != "" {
		return "", fmt.Errorf("%s (%q) is inside the git work tree at %s; "+
			"a screen capture must never be written where it can be committed", chosen, abs, root)
	}
	return abs, nil
}

// repoRootOf returns the work tree dir is inside, or "" if it is in none. It
// walks all the way to the filesystem root: a capture directory three levels
// below a checkout is still in the checkout.
func repoRootOf(dir string) string {
	for d := dir; ; {
		// A .git that is a FILE is a worktree or a submodule, and commits just
		// as well as a directory does.
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

// The guard has to guard.
func TestCaptureDirRefusesTheWorkTree(t *testing.T) {
	// This test file is in one, so its own directory is the case that matters.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if root := repoRootOf(wd); root == "" {
		t.Fatalf("repoRootOf(%q) found no work tree, and this file is in one", wd)
	}
	// Deep inside it, too — the walk must not stop at the first parent.
	if root := repoRootOf(filepath.Join(wd, "testdata", "artifacts")); root == "" {
		t.Error("repoRootOf did not walk up out of testdata/artifacts")
	}
	// And a directory in no work tree must be accepted. The filesystem root is
	// the one place guaranteed not to be a checkout.
	if root := repoRootOf(filepath.VolumeName(wd) + string(filepath.Separator)); root != "" {
		t.Errorf("repoRootOf reported the filesystem root as the work tree %q", root)
	}
}

// And the refusal itself, which is the branch that matters.
func TestChooseCaptureDirRefusesAnythingCommittable(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		want string
	}{
		{"this repository", wd},
		{"the directory the committed frame lives in", filepath.Join(wd, "testdata", "artifacts")},
		{"a path that does not exist yet, inside the tree", filepath.Join(wd, "no", "such", "place")},
		{"a relative path inside the tree", "testdata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chooseCaptureDir(tc.want)
			if err == nil {
				t.Fatalf("chooseCaptureDir(%q) = %q, want a refusal", tc.want, got)
			}
			if !strings.Contains(err.Error(), "never be written where it can be committed") {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		})
	}
	// The default must be usable, or every live run fails on a rule that was
	// meant to redirect it rather than stop it.
	got, err := chooseCaptureDir("")
	if err != nil {
		t.Fatalf("the default capture directory was refused: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("default capture directory %q is not absolute", got)
	}
}
