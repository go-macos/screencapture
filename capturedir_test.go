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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-appdirs/outdir"
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
	dir, err := outdir.Ensure(captureSpec(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("captures go to %s", dir)
	return dir
}

// captureDirEnv overrides where captures go. It is still checked.
const captureDirEnv = "SCREENCAPTURE_ARTIFACT_DIR"

// captureSpec is this repository's answer to "where may a capture go", handed
// to the package that owns the question.
//
// ⛔ This used to be sixty lines here: the default under os.UserConfigDir, the
// walk up for a .git, the refusal. go-appdirs/outdir is the same decision,
// written once, and adopting it FIXED something rather than merely tidying.
// The copy resolved no symbolic links, so a capture directory reached through
// one -- which on macOS is the ordinary case, /tmp being a link to
// /private/tmp -- found no work tree and was accepted. outdir resolves the
// path first and refuses it. Same default directory, so nothing moves.
func captureSpec(want string) outdir.Spec {
	return outdir.Spec{
		App:  "go-macos-screencapture",
		Env:  captureDirEnv,
		Sub:  "captures",
		Want: want,
	}
}

// chooseCaptureDir is the decision, separated from the test plumbing so the
// REFUSAL can be exercised without writing a capture somewhere to find out.
func chooseCaptureDir(want string) (string, error) { return outdir.Choose(captureSpec(want)) }

// repoRootOf is outdir's, kept under this name because the tests below read
// better for it.
func repoRootOf(dir string) string { return outdir.RepoRootOf(dir) }

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
			// ⛔ The assertion is on what the refusal has to TELL somebody,
			// not on its wording. It used to match a phrase this package
			// wrote itself; the phrase now belongs to go-appdirs/outdir and
			// changed, while the behaviour did not. A test that pins prose
			// fails on a rename and passes on a silent change of meaning.
			root := repoRootOf(wd)
			if !strings.Contains(err.Error(), "work tree") ||
				!strings.Contains(err.Error(), root) {
				t.Errorf("the refusal does not name the work tree it found: %v", err)
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

// ⛔ THE HOLE THE LOCAL COPY HAD, and the reason adopting outdir is a fix
// rather than a tidy-up.
//
// The copy walked up from the path as given, resolving nothing. A capture
// directory reached through a symbolic link therefore found no .git and was
// accepted -- and on macOS that is the ordinary case, /tmp being a link to
// /private/tmp. Measured before the change: the copy answered "" for a link
// into a work tree, where outdir answers the tree.
func TestALinkIntoAWorkTreeIsStillTheWorkTree(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(wd, link); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if root := repoRootOf(link); root == "" {
		t.Error("a link into this work tree was not recognised as being in it")
	}
	if _, err := chooseCaptureDir(link); err == nil {
		t.Error("a capture directory reached through a link into a work tree was accepted")
	}
}
