// Package gcpfirewall implements providers.NetworkProvider against GCP Firewall
// Rules Logging, surfacing DENIED connections for an investigation.
//
// GCP "Firewall Rules Logging" records connections evaluated by VPC firewall
// rules that have logging enabled into Cloud Logging, under the log
// "compute.googleapis.com%2Ffirewall", each with a jsonPayload.disposition of
// ALLOWED or DENIED. This provider queries the Cloud Logging API (entries.list)
// for the DENIED ones — the GCP analog of Cilium dropped flows / AWS REJECT
// records. Unlike Cilium Hubble, it is CNI-agnostic: it works on any GCP VPC
// (including GKE) because firewall logging lives in the cloud control plane, not
// in the cluster's data path.
//
// Access is read-only and uses Application Default Credentials by default
// (Workload Identity on GKE, ADC elsewhere); tests inject an HTTP client and
// endpoint instead.
//
// v1 LIMITATION: firewall logs are IP-based. This provider does NOT resolve a
// Selector's namespace/pod/name to IP addresses, so the Selector is ignored —
// every DENIED entry in the window is returned regardless of workload.
package gcpfirewall

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"

	"github.com/Smana/runlore/internal/providers"
)

// defaultMaxEvents bounds how many DENIED entries a single Drops call returns.
const defaultMaxEvents = 100

// firewallLogName is the Cloud Logging log holding Firewall Rules Logging
// entries. The "%2F" is the URL-encoded "/" required inside the logName filter.
const firewallLogName = "compute.googleapis.com%2Ffirewall"

// Client queries GCP Firewall Rules Logging via the Cloud Logging API for a
// single project.
type Client struct {
	svc       *logging.Service
	project   string
	maxEvents int64
}

// New builds a client for the given GCP project. Extra ClientOptions are passed
// straight to the Cloud Logging service constructor: production relies on
// Application Default Credentials (Workload Identity / ADC), while tests inject
// option.WithHTTPClient + option.WithEndpoint + option.WithoutAuthentication.
// It returns an error if project is empty or the service cannot be built.
func New(ctx context.Context, project string, opts ...option.ClientOption) (*Client, error) {
	if project == "" {
		return nil, fmt.Errorf("gcpfirewall: project is required")
	}
	svc, err := logging.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("new logging service: %w", err)
	}
	return &Client{svc: svc, project: project, maxEvents: defaultMaxEvents}, nil
}

var _ providers.NetworkProvider = (*Client)(nil)

// fwPayload is the subset of a Firewall Rules Logging jsonPayload we surface.
type fwPayload struct {
	Disposition string `json:"disposition"`
	Connection  struct {
		SrcIP    string `json:"src_ip"`
		SrcPort  int    `json:"src_port"`
		DestIP   string `json:"dest_ip"`
		DestPort int    `json:"dest_port"`
		Protocol int    `json:"protocol"`
	} `json:"connection"`
	RuleDetails struct {
		Reference string `json:"reference"`
	} `json:"rule_details"`
}

// Drops returns DENIED firewall connections within the window as normalized log
// lines (newest first), capped at the client's max-events budget.
//
// NOTE: firewall logs are IP-based; v1 does not map the Selector's
// namespace/pod/name to IPs, so the selector is ignored — all DENIED entries in
// the window are returned. The window is applied via timestamp filters when set.
func (c *Client) Drops(ctx context.Context, _ providers.Selector, w providers.TimeWindow) (providers.LogResult, error) {
	filter := fmt.Sprintf(
		`logName="projects/%s/logs/%s" AND jsonPayload.disposition="DENIED"`,
		c.project, firewallLogName,
	)
	if !w.Start.IsZero() {
		filter += fmt.Sprintf(` AND timestamp>="%s"`, w.Start.Format(time.RFC3339Nano))
	}
	if !w.End.IsZero() {
		filter += fmt.Sprintf(` AND timestamp<="%s"`, w.End.Format(time.RFC3339Nano))
	}

	// Follow NextPageToken until we collect maxEvents lines or pages are exhausted.
	// A single Entries.List().Do() returns one page (the API may return fewer than
	// PageSize entries plus a token), so without paging a busy window is silently
	// truncated to whatever the first page held. When the cap binds with more
	// available, a sentinel line signals the partial view to the model.
	out := make(providers.LogResult, 0, c.maxEvents)
	truncated := false
	token := ""
	for {
		req := &logging.ListLogEntriesRequest{
			ResourceNames: []string{"projects/" + c.project},
			Filter:        filter,
			OrderBy:       "timestamp desc",
			PageSize:      c.maxEvents,
			PageToken:     token,
		}
		resp, err := c.svc.Entries.List(req).Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("list firewall log entries: %w", err)
		}
		for _, e := range resp.Entries {
			if e == nil {
				continue
			}
			var p fwPayload
			// JsonPayload is a googleapi.RawMessage ([]byte) holding the raw object.
			if err := json.Unmarshal([]byte(e.JsonPayload), &p); err != nil {
				// Skip entries we cannot parse rather than failing the whole query.
				continue
			}
			out = append(out, payloadToLine(p, e.Timestamp))
			if int64(len(out)) >= c.maxEvents {
				// Cap reached: truncated iff there is more to fetch (more on this
				// page is implied by another page token, or this page being full).
				truncated = resp.NextPageToken != ""
				break
			}
		}
		if int64(len(out)) >= c.maxEvents || resp.NextPageToken == "" {
			break
		}
		token = resp.NextPageToken
	}
	if truncated {
		out = append(out, truncationLine(c.maxEvents))
	}
	return out, nil
}

// truncationLine is the sentinel appended when Drops stops at its cap with more
// entries upstream, so the model knows the view is partial. It carries no Time or
// Fields, so it cannot be mistaken for a real flow.
func truncationLine(limit int64) providers.LogLine {
	return providers.LogLine{
		Message: fmt.Sprintf("… results truncated at %d (more matched — narrow the query or shorten the window)", limit),
	}
}

// payloadToLine renders one DENIED firewall connection into a LogLine. ts is the
// entry's RFC3339 timestamp; an empty or unparseable value leaves Time zero.
func payloadToLine(p fwPayload, ts string) providers.LogLine {
	conn := p.Connection
	ruleRef := p.RuleDetails.Reference
	if ruleRef == "" {
		ruleRef = "?"
	}
	line := providers.LogLine{
		Message: fmt.Sprintf("%s:%d -> %s:%d DENIED (%s)",
			conn.SrcIP, conn.SrcPort, conn.DestIP, conn.DestPort, ruleRef),
		Fields: map[string]string{
			"disposition": "DENIED",
			"source":      conn.SrcIP,
			"destination": conn.DestIP,
			"srcport":     strconv.Itoa(conn.SrcPort),
			"destport":    strconv.Itoa(conn.DestPort),
			"protocol":    strconv.Itoa(conn.Protocol),
			"rule":        ruleRef,
		},
	}
	if ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			line.Time = t
		}
	}
	return line
}
