# Contributing

## Dev loop

Prerequisites: Go 1.23+, Docker with Compose, `make`. Optional: `protoc` plus
`protoc-gen-go`/`protoc-gen-go-grpc` only if you change the proto.

```sh
docker compose up -d          # Postgres 16 + Redis 7
make build
make test                     # unit tests; no services required
make test-integration         # Postgres/Redis integration tests
make lint                     # gofmt check + go vet
EVG_STORE=mem ./bin/gateway   # run without Docker at all
```

Regenerate protobuf output after editing `proto/edgevision/v1/gateway.proto`:

```sh
brew install protobuf          # or: apt-get install -y protobuf-compiler
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
make proto
```

Generated files under `internal/pb/` are committed so CI and consumers do not
need `protoc`.

## Before opening a PR

- `make lint && make test && make build` must pass with no containers running.
- Integration tests (`make test-integration`) must pass with compose up.
- New exported symbols in `internal/` need doc comments.
- Keep benchmarks honest: if you change the detector, queue, worker or storage
  path, rerun `make bench` and update `docs/results/benchmark.md` (and the README
  results section) from the generated files. Do not hand-edit measured numbers.
- Add tests behind `//go:build integration` for anything that needs services.
- Use conventional commit messages (`feat:`, `fix:`, `docs:`, `ci:`, `chore:`).

## Design constraints worth preserving

- REST and gRPC must share the same service layer, including shedding and auth.
- Admission must stay non-blocking; never turn a full queue into a blocking
  producer.
- Reads must keep the `source: cache|db` distinction.
- The default build must stay pure Go (`CGO_ENABLED=0`) and compile without an
  ONNX runtime; ONNX changes go behind `//go:build onnx`.
- Do not commit secrets, API keys, or benchmark raw dumps
  (`docs/results/raw/` is ignored on purpose).
