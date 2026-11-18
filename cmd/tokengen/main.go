// Command tokengen prints the bearer token for a simulated device. Tokens are
// deterministic: base64url(deviceID) + "." + hex(HMAC-SHA256(deviceID, secret)).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/edgevision/edgevisiongateway/internal/ingest"
)

func main() {
	device := flag.String("device", "", "device ID (required)")
	secret := flag.String("secret", envOr("EVG_DEVICE_SECRET", "dev-secret-change-me"), "HMAC secret (or EVG_DEVICE_SECRET)")
	flag.Parse()
	if *device == "" {
		fmt.Fprintln(os.Stderr, "usage: tokengen -device cam-1 [-secret ...]")
		os.Exit(1)
	}
	fmt.Println(ingest.IssueToken(*device, *secret))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
