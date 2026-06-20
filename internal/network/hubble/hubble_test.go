package hubble

import (
	"context"
	"net"
	"testing"
	"time"

	flowpb "github.com/cilium/cilium/api/v1/flow"
	observerpb "github.com/cilium/cilium/api/v1/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Smana/runlore/internal/providers"
)

type fakeObserver struct {
	observerpb.UnimplementedObserverServer
	flows []*flowpb.Flow
}

func (s *fakeObserver) GetFlows(_ *observerpb.GetFlowsRequest, stream observerpb.Observer_GetFlowsServer) error {
	for _, f := range s.flows {
		if err := stream.Send(&observerpb.GetFlowsResponse{
			ResponseTypes: &observerpb.GetFlowsResponse_Flow{Flow: f},
		}); err != nil {
			return err
		}
	}
	return nil
}

func TestDrops(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	flow := &flowpb.Flow{
		Time:           timestamppb.New(time.Unix(1700000000, 0)),
		Verdict:        flowpb.Verdict_DROPPED,
		DropReasonDesc: flowpb.DropReason_POLICY_DENIED,
		Source:         &flowpb.Endpoint{Namespace: "apps", PodName: "harbor-core-1"},
		Destination:    &flowpb.Endpoint{Namespace: "db", PodName: "postgres-0"},
	}
	observerpb.RegisterObserverServer(srv, &fakeObserver{flows: []*flowpb.Flow{flow}})
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	dialer := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})
	res, err := New("passthrough:///bufnet", dialer).
		Drops(context.Background(), providers.Selector{Namespace: "apps"}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Drops: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("want 1 flow, got %d", len(res))
	}
	if res[0].Message != "apps/harbor-core-1 -> db/postgres-0 DROPPED (POLICY_DENIED)" {
		t.Fatalf("unexpected message: %q", res[0].Message)
	}
	if res[0].Fields["verdict"] != "DROPPED" || res[0].Fields["drop_reason"] != "POLICY_DENIED" {
		t.Fatalf("unexpected fields: %+v", res[0].Fields)
	}
}
