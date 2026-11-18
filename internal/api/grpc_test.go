package api

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/edgevision/edgevisiongateway/internal/ingest"
	"github.com/edgevision/edgevisiongateway/internal/obs"
	"github.com/edgevision/edgevisiongateway/internal/pb"
	"github.com/edgevision/edgevisiongateway/internal/queue"
	"github.com/edgevision/edgevisiongateway/internal/storage"
)

type grpcFixture struct {
	client pb.GatewayClient
	store  *storage.MemStore
	token  string
}

func newGRPCFixture(t *testing.T, capacity int, threshold float64) *grpcFixture {
	t.Helper()
	metrics := obs.New()
	store := storage.NewMemStore(storage.WithMetrics(metrics), storage.WithHeartbeatTTL(time.Minute))
	if err := store.RegisterDevice(context.Background(), storage.Device{DeviceID: "cam-1"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	q := queue.New(capacity, threshold, metrics)
	svc := ingest.NewService(ingest.ServiceOptions{
		Queue: q, Store: store, Metrics: metrics,
		Recent: ingest.NewRecentTracker(time.Second), CacheWindow: 2 * time.Minute,
	})
	auth := ingest.NewAuthenticator("test-secret", store)
	srv := NewGRPCServer(GRPCOptions{Service: svc, Auth: auth, Store: store, Metrics: metrics})

	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	})
	return &grpcFixture{client: pb.NewGatewayClient(conn), store: store, token: ingest.IssueToken("cam-1", "test-secret")}
}

func authedContext(token string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
}

func TestGRPCRequiresAuth(t *testing.T) {
	f := newGRPCFixture(t, 4, 0.9)
	_, err := f.client.IngestFrame(context.Background(), &pb.IngestFrameRequest{Frame: []byte("x")})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
	_, err = f.client.Heartbeat(context.Background(), &pb.HeartbeatRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("heartbeat code = %v, want Unauthenticated", status.Code(err))
	}
}

func TestGRPCIngestAccepted(t *testing.T) {
	f := newGRPCFixture(t, 4, 0.9)
	resp, err := f.client.IngestFrame(authedContext(f.token), &pb.IngestFrameRequest{
		DeviceId: "cam-1", FrameId: "grpc-frame", Frame: []byte("jpeg"), ContentType: "image/jpeg",
		CapturedAt: timestamppb.Now(),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !resp.Accepted || resp.FrameId != "grpc-frame" || resp.QueueDepth != 1 {
		t.Errorf("response = %+v", resp)
	}
	_, err = f.client.IngestFrame(authedContext(f.token), &pb.IngestFrameRequest{DeviceId: "cam-2", Frame: []byte("x")})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("device mismatch code = %v, want PermissionDenied", status.Code(err))
	}
}

func TestGRPCShedReturnsResourceExhausted(t *testing.T) {
	f := newGRPCFixture(t, 1, 1.0)
	if _, err := f.client.IngestFrame(authedContext(f.token), &pb.IngestFrameRequest{Frame: []byte("x")}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	_, err := f.client.IngestFrame(authedContext(f.token), &pb.IngestFrameRequest{Frame: []byte("x")})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("second ingest code = %v, want ResourceExhausted", status.Code(err))
	}
}

func TestGRPCHeartbeat(t *testing.T) {
	f := newGRPCFixture(t, 4, 0.9)
	resp, err := f.client.Heartbeat(authedContext(f.token), &pb.HeartbeatRequest{DeviceId: "cam-1"})
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !resp.Ok || resp.AliveDevices != 1 {
		t.Errorf("heartbeat = %+v", resp)
	}
}

func TestGRPCQueryDetections(t *testing.T) {
	f := newGRPCFixture(t, 4, 0.9)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := f.store.CacheLatestDetections(ctx, "cam-1", []storage.Detection{{
		DeviceID: "cam-1", FrameID: "cached", CapturedAt: now, Label: "person", Confidence: 0.9, X: 1, Y: 2, W: 3, H: 4,
	}}, time.Minute); err != nil {
		t.Fatalf("cache: %v", err)
	}
	if err := f.store.InsertDetections(ctx, []storage.Detection{{
		DeviceID: "cam-1", FrameID: "db", CapturedAt: now, Label: "object", Confidence: 0.5,
	}}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	resp, err := f.client.QueryDetections(authedContext(f.token), &pb.QueryDetectionsRequest{
		DeviceId: "cam-1",
		From:     timestamppb.New(now.Add(-time.Minute)),
		To:       timestamppb.New(now.Add(time.Minute)),
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if resp.Source != "cache" || len(resp.Detections) != 1 || resp.Detections[0].FrameId != "cached" {
		t.Errorf("cache query = %+v", resp)
	}

	resp, err = f.client.QueryDetections(authedContext(f.token), &pb.QueryDetectionsRequest{
		Label:   "object",
		From:    timestamppb.New(now.Add(-time.Minute)),
		To:      timestamppb.New(now.Add(time.Minute)),
		ForceDb: true,
	})
	if err != nil {
		t.Fatalf("label query: %v", err)
	}
	if resp.Source != "db" || len(resp.Detections) != 1 {
		t.Errorf("label query = %+v", resp)
	}

	_, err = f.client.QueryDetections(authedContext(f.token), &pb.QueryDetectionsRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty query code = %v, want InvalidArgument", status.Code(err))
	}
	_, err = f.client.QueryDetections(authedContext(f.token), &pb.QueryDetectionsRequest{DeviceId: "cam-1", From: timestamppb.New(now), To: timestamppb.New(now)})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("inverted window code = %v, want InvalidArgument", status.Code(err))
	}
}
