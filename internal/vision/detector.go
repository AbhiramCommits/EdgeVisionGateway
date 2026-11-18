// Package vision implements a pluggable object-detection pipeline. The default
// MotionDetector is pure Go (no cgo, no model files); an ONNX Runtime backend
// can be compiled in with `-tags onnx`.
package vision

import (
	"image"
)

// Box is an axis-aligned pixel rectangle: X,Y is the top-left corner.
type Box struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Area returns W*H.
func (b Box) Area() int { return b.W * b.H }

// Detector finds objects in a frame.
//
// Implementations are not required to be safe for concurrent use: the worker
// pool constructs one Detector per worker, which also gives each worker its own
// background model.
type Detector interface {
	// Detect returns the detections found in img.
	Detect(img image.Image) ([]Detection, error)
	// Name identifies the backend and version, e.g. "motion-v1".
	Name() string
}

// Detection is one predicted object with a label, a confidence in [0,1] and a
// pixel box.
type Detection struct {
	Label      string  `json:"label"`
	Confidence float64 `json:"confidence"`
	Box        Box     `json:"box"`
}
