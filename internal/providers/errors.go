// SPDX-License-Identifier: Apache-2.0

package providers

import "errors"

// PermanentError marks a provider/model failure that will not succeed on retry —
// e.g. an HTTP 4xx other than 429 (a malformed request, bad auth, not found). The
// investigation workqueue drops these instead of requeuing with backoff, so a
// doomed request isn't retried forever (which burns model calls and can amplify a
// rate-limit storm). Transient failures (429, 5xx, timeouts, network) are left
// unwrapped so they keep being retried.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }

func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err as non-retryable; nil in ⇒ nil out.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether err — or anything it wraps — is a PermanentError.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// AttemptsError carries how many upstream HTTP requests a failed completion
// cost. A model client retries a transient failure (network error, 429, 5xx)
// before returning, so one Complete can send several requests — each of which
// the provider accepted, processed and BILLED — while the caller sees a single
// error. Without this marker that whole schedule is invisible to any caller
// charging a spend budget, which then bills one request for up to
// clientcore.retryAttempts of them.
//
// It composes with PermanentError rather than replacing it: clientcore marks a
// 4xx permanent and then marks its attempt count, and IsPermanent still sees
// through this wrapper.
type AttemptsError struct {
	Err      error
	Attempts int
}

func (e *AttemptsError) Error() string { return e.Err.Error() }

func (e *AttemptsError) Unwrap() error { return e.Err }

// WithAttempts marks err with the number of upstream requests it cost; nil in ⇒
// nil out. attempts <= 1 returns err unchanged: a single attempt is exactly what
// an unmarked error already means, so the common path allocates nothing.
func WithAttempts(err error, attempts int) error {
	if err == nil || attempts <= 1 {
		return err
	}
	return &AttemptsError{Err: err, Attempts: attempts}
}

// AttemptsOf reports how many upstream requests err — or anything it wraps —
// cost, and 0 when nothing marked it. 0 means "unknown", not "none": a caller
// billing per attempt must read it as at least one.
func AttemptsOf(err error) int {
	var a *AttemptsError
	if errors.As(err, &a) {
		return a.Attempts
	}
	return 0
}

// ErrPRNotOpen reports that a pull/merge request a write was aimed at is no
// longer open — merged, closed, or locked.
//
// It is a SENTINEL rather than a plain error because the caller's recovery
// differs in kind. A transient forge failure means "try the other way of
// recording this" — the thread responder degrades an entry append to a PR
// comment. This means the opposite: the pull request is finished, so a comment
// there is exactly the silent loss the open-check exists to prevent (a comment
// on a merged request is never indexed by the catalog), and the note has to go
// somewhere else entirely — a standalone entry of its own.
//
// It exists because the open-check and the write are two round trips with a
// window between them, and the window is real: a reviewer merging a note PR
// while an on-call is typing the next note into the thread is an ordinary
// Tuesday, not a race that needs contriving. The write call already learns the
// current state from the response it is reading anyway, so reporting it costs
// nothing and closes the window at the only place it can be closed.
var ErrPRNotOpen = errors.New("pull request is not open")
