package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// Matrix delivers via the Matrix client-server send API.
type Matrix struct {
	homeserver string
	roomID     string
	token      string
	http       *http.Client
	txn        atomic.Int64
}

// NewMatrix builds a Matrix notifier. homeserver is the base URL (e.g.
// https://matrix.org); roomID is like "!abc:hs"; token is an access token.
func NewMatrix(homeserver, roomID, token string) *Matrix {
	return &Matrix{
		homeserver: strings.TrimRight(homeserver, "/"),
		roomID:     roomID,
		token:      token,
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

var _ providers.Notifier = (*Matrix)(nil)

// Deliver sends the formatted investigation as an m.notice message.
func (m *Matrix) Deliver(ctx context.Context, inv providers.Investigation) error {
	txn := fmt.Sprintf("runlore-%d", m.txn.Add(1))
	endpoint := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		m.homeserver, url.PathEscape(m.roomID), url.PathEscape(txn))

	body, err := json.Marshal(map[string]string{"msgtype": "m.notice", "body": Format(inv)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.token)
	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("matrix send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("matrix status %d", resp.StatusCode)
	}
	return nil
}
