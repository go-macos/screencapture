// Copyright (c) the go-macos/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package screencapture

import "github.com/go-macos/objc"

// Objective-C blocks, in one place.
//
// Three ScreenCaptureKit entry points are asynchronous and take a completion
// block: +getShareableContentWithCompletionHandler:,
// -startCaptureWithCompletionHandler: and -stopCaptureWithCompletionHandler:.
// A block's Go implementation takes the block itself as its FIRST parameter —
// that is the block ABI, and the runtime bridge panics if the first parameter
// is anything else.
type block = objc.Block

// newBlock wraps a Go function as an Objective-C block this package owns.
func newBlock(fn any) blockHandle { return blockHandle(objc.NewBlock(fn)) }

// blockHandle is a block whose lifetime this package manages. The bridge keeps
// the Go closure alive in a process-wide cache keyed by the block pointer, so
// a block that is never released leaks that closure; every asynchronous call
// here releases its block once the completion handler has run.
type blockHandle objc.Block

// release drops the block's reference, freeing the cached Go closure.
func (b blockHandle) release() { objc.Block(b).Release() }
