// Package hostplatform wraps the native desktop capabilities Vantry needs but
// the Go standard library does not expose: an idle-sleep inhibitor for long
// background runs, native desktop notifications, and reading an image off the
// system clipboard.
//
// Everything here is best effort. A host without the native tool degrades to a
// no-op instead of failing a run, and a clipboard holding text reports "not
// found" rather than an error. External commands are always started with an
// argument vector and never through a shell; payloads that come from runs,
// thread titles, or model output are passed as argv, so quotes, backslashes,
// newlines, or script statements in a payload cannot turn into code.
package hostplatform
