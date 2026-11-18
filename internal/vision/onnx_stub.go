//go:build !onnx

package vision

import "errors"

// newONNXDetector is the stub used when the binary is built without the onnx
// build tag. FromConfig logs a warning and falls back to MotionDetector.
func newONNXDetector(string) (Detector, error) {
	return nil, errors.New("vision: this binary was built without -tags onnx")
}
