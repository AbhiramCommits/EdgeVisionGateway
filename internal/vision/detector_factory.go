package vision

import (
	"errors"
	"log/slog"
)

// FromConfig selects a detector backend. "onnx" requires a binary built with
// -tags onnx and the runtime shared library installed; any failure to create it
// is logged as a warning and the pure-Go MotionDetector is used instead, so a
// misconfigured deployment still ingests frames.
func FromConfig(backend, modelPath string) (Detector, error) {
	if backend == "onnx" {
		d, err := newONNXDetector(modelPath)
		if err == nil {
			slog.Info("detector backend selected", "backend", "onnx", "model", modelPath)
			return d, nil
		}
		slog.Warn("onnx detector unavailable, falling back to motion", "err", err)
		return NewMotionDetector(), nil
	}
	if backend != "" && backend != "motion" {
		return nil, errors.New("vision: unknown detector backend " + backend)
	}
	return NewMotionDetector(), nil
}
