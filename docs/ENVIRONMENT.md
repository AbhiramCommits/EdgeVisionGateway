# Environment

Every performance and accuracy number in this repository was measured on the
machine described below. These values were captured with:

```sh
go version
uname -a
sysctl -n hw.ncpu
sysctl -n hw.memsize
docker info
```

## Host

| Item | Value |
| --- | --- |
| Date captured | 2026-11-17 |
| OS | macOS 27.2.1 (Darwin 27.2.0, arm64) |
| CPU | Apple M2 (arm64), 8 logical cores (`sysctl -n hw.ncpu` → `8`) |
| RAM | 16 GiB (`hw.memsize` → `17179869184` bytes) |
| Disk free at start | ~97 GiB |
| Shell | zsh |

Raw output:

```text
$ go version
go version go1.27.1 darwin/arm64

$ uname -a
Darwin Abhirams-MacBook-Air.local 27.2.0 Darwin Kernel Version 27.2.0: Wed Oct 14 23:43:44 PDT 2026; root:xnu-13432.41.4~25/RELEASE_ARM64_T8122 arm64

$ sysctl -n hw.ncpu
8

$ sysctl -n hw.memsize
17179869184
```

## Toolchain

| Tool | Version / path |
| --- | --- |
| Go | go1.27.1 darwin/arm64 (module language version: go 1.23) |
| Docker | yes — Docker Desktop 29.8.2 |
| Docker VM | 8 CPUs, 8,319,770,624 bytes RAM (≈7.75 GiB) |
| protoc | `/opt/homebrew/bin/protoc` |
| kubectl | `/usr/local/bin/kubectl` |
| terraform | `/opt/homebrew/bin/terraform` |
| psql | `/opt/homebrew/bin/psql` |
| redis-cli | `/opt/homebrew/bin/redis-cli` |

Docker verdict: **usable**. Postgres 16 and Redis 7 run as containers from
`docker-compose.yml`, so the storage integration tests and the benchmark ran
against real services, not the in-memory fallback.

Caveat for reading results: the Docker VM was allocated 8 vCPUs and ~7.7 GiB,
which is less than the host's full 16 GiB. Containerised Postgres/Redis
therefore competed with the gateway and load generator for the same 8 cores.
