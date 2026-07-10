// SPDX-License-Identifier: Apache-2.0

// Package hubble implements providers.NetworkProvider against Cilium Hubble Relay
// (the observer gRPC API), surfacing dropped flows for an investigation.
package hubble

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	flowpb "github.com/cilium/cilium/api/v1/flow"
	observerpb "github.com/cilium/cilium/api/v1/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Smana/runlore/internal/providers"
)

const maxFlows = 100

// Client queries a Hubble Relay endpoint (e.g. hubble-relay.kube-system:80).
type Client struct {
	addr string
	opts []grpc.DialOption
}

// New builds a client. Extra dial options are appended (used by tests for an
// in-memory connection); production uses insecure transport to the relay.
func New(addr string, opts ...grpc.DialOption) *Client {
	return &Client{addr: addr, opts: opts}
}

var _ providers.NetworkProvider = (*Client)(nil)

// Drops returns DROPPED flows touching the selector within the window.
func (c *Client) Drops(ctx context.Context, sel providers.Selector, w providers.TimeWindow) (providers.LogResult, error) {
	dialOpts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, c.opts...)
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
