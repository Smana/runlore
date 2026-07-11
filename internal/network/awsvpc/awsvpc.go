// SPDX-License-Identifier: Apache-2.0

// Package awsvpc implements providers.NetworkProvider against AWS VPC Flow Logs
// delivered to a CloudWatch Logs group, surfacing REJECT (denied) flows for an
// investigation. All calls are read-only (FilterLogEvents).
//
// It is CNI-agnostic: it works on any AWS VPC — including EKS clusters running the
// AWS VPC CNI — unlike Cilium Hubble, which requires the Cilium data plane. Auth
// uses the SDK's default credential chain (EKS Pod Identity / IRSA / env / profile),
// the same mechanism as internal/providers/cloud/aws.
//
// VPC Flow Logs are IP-based: a flow record carries source/destination IPs, not
// Kubernetes namespaces or pod names. This v1 therefore does NOT resolve the
// Selector's namespace/pod down to IPs — Drops ignores the Selector and returns
// recent VPC-wide REJECTs. Mapping IPs back to workloads is left for a later pass.
package awsvpc

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"

	"github.com/Smana/runlore/internal/providers"
)

// rejectFilterPattern is a CloudWatch Logs space-delimited filter selecting REJECT
// records in the default VPC flow-log v2 format. The field names are positional
// labels; only action=REJECT constrains the match.
const rejectFilterPattern = "[version, account, eni, source, destination, srcport, destport, protocol, packets, bytes, start, end, action=REJECT, logstatus]"

// defaultMaxEvents caps the number of REJECT events Drops returns (and paginates to).
const defaultMaxEvents = 100

// cwlAPI is the narrow CloudWatch Logs surface Drops needs, so tests can inject a
// fake and the real *cloudwatchlogs.Client satisfies it directly.
type cwlAPI interface {
	FilterLogEvents(ctx context.Context, in *cloudwatchlogs.FilterLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error)
}

// Client queries a CloudWatch Logs group that receives AWS VPC Flow Logs.
type Client struct {
	cwl       cwlAPI
	logGroup  string // the CloudWatch Logs group flow logs are delivered to
	maxEvents int    // cap on REJECT events returned
}

// New builds a Client from the default AWS credential chain (Pod Identity / IRSA /
// env / profile). region may be empty (resolved from the environment/IMDS).
// logGroup is the CloudWatch Logs group VPC Flow Logs are delivered to and must be
// non-empty.
func New(ctx context.Context, region, logGroup string) (*Client, error) {
	if logGroup == "" {
		return nil, fmt.Errorf("awsvpc: logGroup must not be empty")
	}
	opts := []func(*config.LoadOptions) error{}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("awsvpc: load aws config: %w", err)
	}
	return &Client{
		cwl:       cloudwatchlogs.NewFromConfig(cfg),
		logGroup:  logGroup,
		maxEvents: defaultMaxEvents,
	}, nil
}

var _ providers.NetworkProvider = (*Client)(nil)

// scopingNote returns a synthetic LogLine that makes the IP-based, VPC-wide scope
// of this provider explicit to the consuming model. It appears FIRST in every Drops
// result so the model cannot attribute internet-scanner REJECT noise to the workload
// under investigation without first correlating by IP. When sel carries a namespace
// or name the note includes them so the model knows what it should look for.
func scopingNote(sel providers.Selector) providers.LogLine {
	scope := "<namespace>/<pod>"
	switch {
	case sel.Namespace != "" && sel.Name != "":
		scope = sel.Namespace + "/" + sel.Name
	case sel.Namespace != "":
		scope = sel.Namespace + "/<pod>"
	case sel.Name != "":
		scope = "<namespace>/" + sel.Name
	}
	return providers.LogLine{
		Message: fmt.Sprintf(
			"NOTE: source is IP-based (VPC/subnet flow logs); results are VPC-wide and NOT scoped to %s"+
				" — correlate by IP (see pod IPs in pod_status) before attributing.",
			scope,
		),
	}
}

// Drops returns recent VPC-wide REJECT flows within the window.
//
// sel is not used to filter CloudWatch: VPC Flow Logs are IP-based and this v1
// does not map the selector's namespace/pod to IPs, so Drops cannot scope to a
// workload (see the package doc). A scoping note is always prepended as the first
// LogLine so the model sees the IP-based, VPC-wide limitation before any results.
// It calls FilterLogEvents with the REJECT filter pattern, following NextToken pages
// until maxEvents events are collected or pagination is exhausted, parsing each
// space-delimited flow-log v2 message into a normalized LogLine.
func (c *Client) Drops(ctx context.Context, sel providers.Selector, w providers.TimeWindow) (providers.LogResult, error) {
	// Clamp into int32 range before the cast so the conversion is provably safe
	// (maxEvents is a small positive cap, but gosec G115 can't prove it).
	limit := int32(math.MaxInt32)
	if c.maxEvents >= 0 && c.maxEvents < math.MaxInt32 {
		limit = int32(c.maxEvents)
	}
	in := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName:  &c.logGroup,
		FilterPattern: ptr(rejectFilterPattern),
		Limit:         &limit,
	}
	if !w.Start.IsZero() {
		in.StartTime = ptr(w.Start.UnixMilli())
	}
	if !w.End.IsZero() {
		in.EndTime = ptr(w.End.UnixMilli())
	}

	// Prepend the scoping note so it is always the first entry the model sees,
	// regardless of how many flow lines follow (or whether the window is empty).
	// flowCount tracks only parsed flow lines so the maxEvents cap applies to
	// real REJECT entries, not the synthetic note.
	out := providers.LogResult{scopingNote(sel)}
	flowCount := 0
	for {
		resp, err := c.cwl.FilterLogEvents(ctx, in)
		if err != nil {
			return out, fmt.Errorf("awsvpc: filter log events: %w", err)
		}
		for i := range resp.Events {
			line, ok := parseFlowLine(resp.Events[i])
			if !ok {
				continue // malformed/short record
			}
			out = append(out, line)
			flowCount++
			if flowCount >= c.maxEvents {
				return out, nil
			}
		}
		if resp.NextToken == nil || *resp.NextToken == "" {
			break
		}
		in.NextToken = resp.NextToken
	}
	return out, nil
}

// parseFlowLine parses one VPC flow-log v2 default-format event into a LogLine.
// The default format is space-delimited; Drops filters for action=REJECT, so a
// well-formed record always carries that verdict. Records with fewer than 13 fields
// are malformed and rejected (ok=false).
func parseFlowLine(ev cwltypes.FilteredLogEvent) (providers.LogLine, bool) {
	msg := ""
	if ev.Message != nil {
		msg = *ev.Message
	}
	f := strings.Fields(msg)
	if len(f) < 13 {
		return providers.LogLine{}, false
	}
	// Field indices in the v2 default format:
	// 3=srcaddr 4=dstaddr 5=srcport 6=dstport 7=protocol 12=action.
	srcaddr, dstaddr := f[3], f[4]
	srcport, dstport := f[5], f[6]
	protocol := f[7]
	line := providers.LogLine{
		Message: fmt.Sprintf("%s:%s -> %s:%s REJECT (proto %s)", srcaddr, srcport, dstaddr, dstport, protocol),
		Fields: map[string]string{
			"action":      "REJECT",
			"source":      srcaddr,
			"destination": dstaddr,
			"srcport":     srcport,
			"dstport":     dstport,
			"protocol":    protocol,
		},
	}
	if ev.Timestamp != nil {
		line.Time = time.UnixMilli(*ev.Timestamp)
	}
	return line, true
}

// ptr is a small helper for the SDK's pointer-heavy inputs.
func ptr[T any](v T) *T { return &v }
