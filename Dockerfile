# Multi-stage build: Go 1.23 builder (module language version is 1.23) and a
# distroless static runtime. CGO is disabled, so the binary is fully static and
# runs on distroless/static.
#
# The image has no shell, curl or package manager by design. The health check
# equivalent is an HTTP probe against /healthz (liveness) and /readyz
# (readiness); see deploy/k8s/gateway-deployment.yaml. With plain Docker, run:
#   docker run -p 8080:8080 -p 9090:9090 edgevisiongateway:dev
#   curl -f http://localhost:8080/healthz
FROM golang:1.23-alpine AS build

WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/gateway /gateway
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/gateway"]
