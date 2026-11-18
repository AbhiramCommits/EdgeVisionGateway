# EdgeVisionGateway

A Go backend that ingests image frames and heartbeats from a simulated fleet of edge cameras, runs a pure-Go computer-vision detector in a bounded worker pool, persists detections, and serves low-latency query APIs.
