# Local CI-equivalent run

Everything below is real output from this machine (see
`docs/ENVIRONMENT.md`). GitHub Actions (`ci.yml`) runs the same commands on
`ubuntu-latest` with `go-version-file: go.mod` (Go 1.23).

| Check | Command | Result |
| --- | --- | --- |
| gofmt | `gofmt -l .` | clean (no files listed) |
| vet | `go vet ./...` | pass |
| unit tests + race | `go test -race -coverprofile=coverage.out -covermode=atomic ./...` | all packages pass |
| unit coverage | `go tool cover -func=coverage.out \| tail -1` | **47.4% of statements** |
| test functions | `grep -rh '^func Test' --include='*_test.go' . \| wc -l` | **87** |
| integration | `go test -tags integration -v ./internal/storage/...` | **15/15 pass** (8 memstore, 4 Postgres, 3 Redis) |
| combined coverage | `go test -tags integration -coverprofile=coverage-integration.out ./...` | **52.2% of statements** |
| Go 1.23 compatibility | `docker run --rm -v "$PWD":/src -w /src golang:1.23-alpine go test -mod=readonly ./...` | all packages pass |
| image build | `docker build -t edgevisiongateway:dev .` | success, **30,156,467 bytes** |
| terraform fmt | `terraform fmt -check` (in `terraform/`) | pass |
| terraform init | `terraform init -backend=false` | success |
| terraform validate | `terraform validate` | `Success! The configuration is valid.` |
| k8s dry run | `kubectl apply --dry-run=client -k deploy/k8s` | **10 resources created (dry run)** |

Coverage note: the unit-run number excludes `internal/storage`'s Postgres and
Redis code, which requires running services and is compiled only under
`-tags integration`; that is why the same suite measures 72.5% in
`internal/storage` and 52.2% overall once integration tests are included.
Generated protobuf code and the `cmd/*` binaries contain no tests, which pulls
the repository-wide percentage down further.

Representative raw output:

```console
$ go test -race -coverprofile=coverage.out -covermode=atomic ./...
ok  github.com/edgevision/edgevisiongateway/internal/api       coverage: 78.8% of statements
ok  github.com/edgevision/edgevisiongateway/internal/config    coverage: 70.1% of statements
ok  github.com/edgevision/edgevisiongateway/internal/ingest    coverage: 57.4% of statements
ok  github.com/edgevision/edgevisiongateway/internal/obs       coverage: 97.3% of statements
ok  github.com/edgevision/edgevisiongateway/internal/queue     coverage: 90.5% of statements
ok  github.com/edgevision/edgevisiongateway/internal/storage   coverage: 34.6% of statements
ok  github.com/edgevision/edgevisiongateway/internal/vision    coverage: 89.2% of statements
total: (statements) 47.4%

$ go test -count=1 -tags integration -v ./internal/storage/...
--- PASS: TestPostgresPartitionRouting (0.02s)
--- PASS: TestPostgresMigrateCreatesDefaultPartitions (0.01s)
--- PASS: TestRedisHeartbeatTTLAndAliveCount (0.00s)
--- PASS: TestRedisHotCacheTTLAndMetrics (1.20s)
PASS
ok  github.com/edgevision/edgevisiongateway/internal/storage  1.514s
```
