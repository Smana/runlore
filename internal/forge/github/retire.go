// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"errors"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// ErrAlreadyRetired signals the entry already carries status:retired on the base
// branch, so no retirement PR is needed. The curate pass treats it as done-skip.
var ErrAlreadyRetired = errors.New("entry already retired on base branch")

// setStatusRetired stamps `status: retired` into an OKF entry's YAML frontmatter,
// editing ONLY the status line — human formatting, key order and comments are
// preserved (this file is a human-authored artifact under review; a re-marshal
// would produce an unreadable retirement diff). Scanning is fence-bounded so a
// "status:" string in the markdown body is never touched. already=true means the
// entry is retired on the base branch and no PR is needed. A file without a
// frontmatter block errors: retirement must never write blind.
func setStatusRetired(content []byte) (out []byte, already bool, err error) {
	lines, rest, err := frontmatterBlock(content)
	if err != nil {
		return nil, false, err
	}
	for i, ln := range lines {
		if key, val, ok := strings.Cut(ln, ":"); ok && strings.TrimSpace(key) == "status" {
			if strings.TrimSpace(val) == "retired" {
				return content, true, nil
			}
			lines[i] = "status: retired"
			return []byte("---\n" + strings.Join(lines, "\n") + rest), false, nil
		}
	}
	return []byte("---\nstatus: retired\n" + strings.Join(lines, "\n") + rest), false, nil
}

// retireLabels mark a PR proposed by the curate retirement pass — "runlore" for
// the shared forge namespace, "runlore-retire" for the pass's idempotency and
// human-veto listings.
var retireLabels = []string{"runlore", "runlore-retire"}

// OpenRetirePR opens a human-reviewed PR that stamps status:retired into an
// existing catalog entry's frontmatter. It never merges and never deletes — a
// human is the load-bearing gate. body carries the reviewer-facing track record
// and the hidden idempotency marker (authored by the caller). Returns
// ErrAlreadyRetired when the entry is already retired on the base branch (no PR
// opened); a 404 on the entry file surfaces as an error (entry deleted → the pass
// logs and skips it).
func (c *Client) OpenRetirePR(ctx context.Context, entryPath, body string) (providers.Ref, error) {
	return c.openEntryEditPR(ctx, entryEdit{
		path: entryPath,
		stamp: func(raw []byte) ([]byte, error) {
			out, already, err := setStatusRetired(raw)
			if err != nil {
				return nil, err
			}
			if already {
				return nil, ErrAlreadyRetired
			}
			return out, nil
		},
		branchPrefix: "retire",
		commitVerb:   "retire",
		titlePrefix:  "KB retire: ",
		labels:       retireLabels,
		body:         body,
	})
}
