// SPDX-License-Identifier: Apache-2.0

// Package hubble implements providers.NetworkProvider against Cilium Hubble Relay
// (the observer gRPC API), surfacing dropped flows for an investigation.
package hubble

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"strings"

	flowpb "github.com/cilium/cilium/api/v1/flow"
	observerpb "github.com/cilium/cilium/api/v1/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Smana/runlore/internal/providers"
)

const maxFlows = 100

// Client queries a Hubble Relay endpoint (e.g. hubble-relay.kube-system:80).
type Client struct {
	addr    string
	opts    []grpc.DialOption
	tlsMode bool // false (default) = insecure/plaintext; true = TLS
}

// New builds a client. tlsEnabled selects TLS transport (credentials.NewTLS)
// when true, or insecure/plaintext when false (the DEFAULT — keeps the
// maintainer's test cluster connecting without any config change).
// Extra dial options are appended and take precedence (used by tests for an
// in-memory connection).
func New(addr string, tlsEnabled bool, opts ...grpc.DialOption) *Client {
	return &Client{addr: addr, tlsMode: tlsEnabled, opts: opts}
}

var _ providers.NetworkProvider = (*Client)(nil)

// tlsConfig is the client TLS configuration for Hubble Relay dials. MinVersion
// is pinned to TLS 1.2 — the zero-value tls.Config would accept TLS 1.0/1.1.
func tlsConfig() *tls.Config { return &tls.Config{MinVersion: tls.VersionTLS12} }

// Drops returns DROPPED flows touching the selector within the window.
func (c *Client) Drops(ctx context.Context, sel providers.Selector, w providers.TimeWindow) (providers.LogResult, error) {
	// Select transport credentials: insecure/plaintext (the DEFAULT) or TLS.
	var transportCreds grpc.DialOption
	if c.tlsMode {
		transportCreds = grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig()))
	} else {
		transportCreds = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	dialOpts := append([]grpc.DialOption{transportCreds}, c.opts...)
	conn, err := grpc.NewClient(c.addr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial hubble: %w", err)
	}
	defer func() { _ = conn.Close() }()

	req := &observerpb.GetFlowsRequest{
		Number:    maxFlows,
		Whitelist: dropFilters(sel),
	}
	if !w.Start.IsZero() {
		req.Since = timestamppb.New(w.Start)
	}
	if !w.End.IsZero() {
		req.Until = timestamppb.New(w.End)
	}

	stream, err := observerpb.NewObserverClient(conn).GetFlows(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("get flows: %w", err)
	}
	var out providers.LogResult
	flowCount := 0
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, fmt.Errorf("recv flow: %w", err)
		}
		if f := resp.GetFlow(); f != nil {
			out = append(out, flowToLine(f))
			flowCount++
			if flowCount >= maxFlows {
				// Cap reached: append the truncation sentinel so the model knows
				// the view is partial (mirrors the other flow sources).
				out = append(out, providers.TruncationLine(int64(maxFlows)))
				break
			}
		}
	}
	return out, nil
}

// dropFilters builds the flow whitelist: always DROPPED, optionally scoped to the
// selector's namespace/pod on either side (two entries = OR across source/dest).
func dropFilters(sel providers.Selector) []*flowpb.FlowFilter {
	dropped := []flowpb.Verdict{flowpb.Verdict_DROPPED}
	if sel.Namespace == "" {
		return []*flowpb.FlowFilter{{Verdict: dropped}}
	}
	prefix := sel.Namespace + "/"
	if sel.Name != "" {
		prefix += sel.Name
	}
	return []*flowpb.FlowFilter{
		{Verdict: dropped, SourcePod: []string{prefix}},
		{Verdict: dropped, DestinationPod: []string{prefix}},
	}
}

func flowToLine(f *flowpb.Flow) providers.LogLine {
	src, dst := endpoint(f.GetSource()), endpoint(f.GetDestination())
	reason := f.GetDropReasonDesc().String()
	line := providers.LogLine{
		Message: fmt.Sprintf("%s -> %s DROPPED (%s)", src, dst, reason),
		Fields: map[string]string{
			"verdict":     f.GetVerdict().String(),
			"drop_reason": reason,
			"source":      src,
			"destination": dst,
		},
	}
	if t := f.GetTime(); t != nil {
		line.Time = t.AsTime()
	}
	return line
}

func endpoint(ep *flowpb.Endpoint) string {
	if ep == nil {
		return "?"
	}
	if ep.GetPodName() != "" {
		return ep.GetNamespace() + "/" + ep.GetPodName()
	}
	if ep.GetNamespace() != "" {
		return ep.GetNamespace()
	}
	return strings.Join(ep.GetLabels(), ",")
}
