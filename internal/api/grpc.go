package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/edgevision/edgevisiongateway/internal/ingest"
	"github.com/edgevision/edgevisiongateway/internal/obs"
	"github.com/edgevision/edgevisiongateway/internal/pb"
	"github.com/edgevision/edgevisiongateway/internal/storage"
)

// GRPCOptions configures the gRPC transport.
type GRPCOptions struct {
	Service *ingest.Service
	Auth    *ingest.Authenticator
	Store   storage.Store
	Metrics *obs.Metrics
	Logger  *slog.Logger
}

type gatewayGRPC struct {
	pb.UnimplementedGatewayServer
	svc     *ingest.Service
	store   storage.Store
	metrics *obs.Metrics
	log     *slog.Logger
}

// NewGRPCServer builds a gRPC server with authentication as a unary
// interceptor. Every RPC therefore has a verified device ID in its context.
func NewGRPCServer(o GRPCOptions) *grpc.Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	srv := grpc.NewServer(grpc.UnaryInterceptor(authInterceptor(o.Auth, o.Metrics)))
	pb.RegisterGatewayServer(srv, &gatewayGRPC{
		svc:     o.Service,
		store:   o.Store,
		metrics: o.Metrics,
		log:     o.Logger,
	})
	return srv
}

type deviceIDKey struct{}

func authInterceptor(auth *ingest.Authenticator, metrics *obs.Metrics) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		token := ""
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get("authorization"); len(vals) > 0 {
				token = bearerToken(vals[0])
			}
		}
		deviceID, err := auth.Authenticate(ctx, token)
		if err != nil {
			if metrics != nil {
				metrics.IncAuthFailures()
			}
			return nil, status.Error(codes.Unauthenticated, "invalid device token")
		}
		return handler(context.WithValue(ctx, deviceIDKey{}, deviceID), req)
	}
}

func deviceFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(deviceIDKey{}).(string); ok {
		return v
	}
	return ""
}

// IngestFrame implements pb.GatewayServer.
func (g *gatewayGRPC) IngestFrame(ctx context.Context, req *pb.IngestFrameRequest) (*pb.IngestFrameResponse, error) {
	device := deviceFromContext(ctx)
	if req.GetDeviceId() != "" && req.GetDeviceId() != device {
		return nil, status.Error(codes.PermissionDenied, "token does not match device_id")
	}
	if len(req.GetFrame()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "frame is required")
	}
	if len(req.GetFrame()) > 8<<20 {
		return nil, status.Error(codes.InvalidArgument, "frame exceeds 8 MiB")
	}
	var capturedAt time.Time
	if ts := req.GetCapturedAt(); ts != nil {
		capturedAt = ts.AsTime()
	}
	frameID := req.GetFrameId()
	if frameID == "" {
		frameID = "f-" + randomID()
	}
	accepted, out, err := g.svc.IngestFrame(ctx, device, frameID, capturedAt, req.GetFrame(), req.GetContentType())
	if err != nil {
		if errors.Is(err, ingest.ErrShuttingDown) {
			return nil, status.Error(codes.Unavailable, "gateway is shutting down")
		}
		g.log.Error("grpc ingest failed", "device_id", device, "err", err)
		return nil, status.Error(codes.Internal, "ingest failed")
	}
	if !accepted {
		_ = grpc.SetHeader(ctx, metadata.Pairs("retry-after-ms", "1000"))
		return nil, status.Error(codes.ResourceExhausted, "queue saturated: frame shed")
	}
	return &pb.IngestFrameResponse{
		Accepted:     true,
		FrameId:      out.FrameID,
		QueueDepth:   int32(out.QueueDepth),
		RetryAfterMs: int32(out.RetryAfter.Milliseconds()),
	}, nil
}

// Heartbeat implements pb.GatewayServer.
func (g *gatewayGRPC) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	device := deviceFromContext(ctx)
	if req.GetDeviceId() != "" && req.GetDeviceId() != device {
		return nil, status.Error(codes.PermissionDenied, "token does not match device_id")
	}
	if err := g.svc.Heartbeat(ctx, device); err != nil {
		if errors.Is(err, ingest.ErrShuttingDown) {
			return nil, status.Error(codes.Unavailable, "gateway is shutting down")
		}
		g.log.Error("grpc heartbeat failed", "device_id", device, "err", err)
		return nil, status.Error(codes.Internal, "heartbeat failed")
	}
	alive, err := g.store.CountAlive(ctx, time.Now().UTC())
	if err != nil {
		alive = 0
	}
	return &pb.HeartbeatResponse{Ok: true, AliveDevices: int64(alive)}, nil
}

// QueryDetections implements pb.GatewayServer.
func (g *gatewayGRPC) QueryDetections(ctx context.Context, req *pb.QueryDetectionsRequest) (*pb.QueryDetectionsResponse, error) {
	var from, to time.Time
	if ts := req.GetFrom(); ts != nil {
		from = ts.AsTime()
	}
	if ts := req.GetTo(); ts != nil {
		to = ts.AsTime()
	}
	limit := int(req.GetLimit())
	switch {
	case req.GetDeviceId() != "":
		res, err := g.svc.QueryDevice(ctx, req.GetDeviceId(), from, to, limit, req.GetForceDb())
		if err != nil {
			return nil, mapQueryErr(err)
		}
		return &pb.QueryDetectionsResponse{Detections: toProto(res.Detections), Source: res.Source}, nil
	case req.GetLabel() != "":
		dets, err := g.svc.QueryLabel(ctx, req.GetLabel(), from, to, limit)
		if err != nil {
			return nil, mapQueryErr(err)
		}
		return &pb.QueryDetectionsResponse{Detections: toProto(dets), Source: "db"}, nil
	default:
		return nil, status.Error(codes.InvalidArgument, "device_id or label is required")
	}
}

func mapQueryErr(err error) error {
	if errors.Is(err, storage.ErrInvalidRange) {
		return status.Error(codes.InvalidArgument, "from must be before to")
	}
	return status.Error(codes.Internal, fmt.Sprintf("query failed: %v", err))
}

func toProto(dets []storage.Detection) []*pb.Detection {
	out := make([]*pb.Detection, 0, len(dets))
	for _, d := range dets {
		out = append(out, &pb.Detection{
			DeviceId:   d.DeviceID,
			FrameId:    d.FrameID,
			CapturedAt: timestamppb.New(d.CapturedAt),
			Label:      d.Label,
			Confidence: d.Confidence,
			X:          int32(d.X),
			Y:          int32(d.Y),
			W:          int32(d.W),
			H:          int32(d.H),
		})
	}
	return out
}
