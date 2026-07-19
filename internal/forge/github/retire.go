// SPDX-License-Identifier: Apache-2.0

package github

import (
	"fmt"
	"strings"
)

// setStatusRetired stamps `status: retired` into an OKF entry's YAML frontmatter,
// editing ONLY the status line — human formatting, key order and comments are
// preserved (this file is a human-authored artifact under review; a re-marshal
// would produce an unreadable retirement diff). Scanning is fence-bounded so a
// "status:" string in the markdown body is never touched. already=true means the
// entry is retired on the base branch and no PR is needed. A file without a
// frontmatter block errors: retirement must never write blind.
func setStatusRetired(content []byte) (out []byte, already bool, err error) {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") {
		return nil, false, fmt.Errorf("entry has no YAML frontmatter block")
	}
	rest := s[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, false, fmt.Errorf("entry frontmatter block is unterminated")
	}
	fm, body := rest[:end], rest[end:]
	lines := strings.Split(fm, "\n")
	for i, ln := range lines {
		if key, val, ok := strings.Cut(ln, ":"); ok && strings.TrimSpace(key) == "status" {
			if strings.TrimSpace(val) == "retired" {
				return content, true, nil
			}
			lines[i] = "status: retired"
			return []byte("---\n" + strings.Join(lines, "\n") + body), false, nil
		}
	}
	return []byte("---\nstatus: retired\n" + fm + body), false, nil
}
