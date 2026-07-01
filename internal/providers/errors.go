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
