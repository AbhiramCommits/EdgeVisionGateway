//go:build onnx

// This file is only compiled with `-tags onnx`. It requires the ONNX Runtime
// shared library at runtime and a YOLOv8-style model (input [1,3,640,640],
// output [1,4+classes,anchors]).
package vision

import (
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

const onnxInputSize = 640

var (
	onnxInitOnce sync.Once
	onnxInitErr  error
	cocoLabels   = []string{
		"person", "bicycle", "car", "motorcycle", "airplane", "bus", "train", "truck", "boat",
		"traffic light", "fire hydrant", "stop sign", "parking meter", "bench", "bird", "cat",
		"dog", "horse", "sheep", "cow", "elephant", "bear", "zebra", "giraffe", "backpack",
		"umbrella", "handbag", "tie", "suitcase", "frisbee", "skis", "snowboard", "sports ball",
		"kite", "baseball bat", "baseball glove", "skateboard", "surfboard", "tennis racket",
		"bottle", "wine glass", "cup", "fork", "knife", "spoon", "bowl", "banana", "apple",
		"sandwich", "orange", "broccoli", "carrot", "hot dog", "pizza", "donut", "cake", "chair",
		"couch", "potted plant", "bed", "dining table", "toilet", "tv", "laptop", "mouse",
		"remote", "keyboard", "cell phone", "microwave", "oven", "toaster", "sink",
		"refrigerator", "book", "clock", "vase", "scissors", "teddy bear", "hair drier",
		"toothbrush",
	}
)

// ONNXDetector runs a YOLO-style ONNX model through ONNX Runtime. It owns a
// single session and reusable input/output tensors, so it must not be used
// concurrently; the worker pool creates one instance per worker.
type ONNXDetector struct {
	session    *ort.AdvancedSession
	input      *ort.Tensor[float32]
	output     *ort.Tensor[float32]
	inputData  []float32
	confThresh float32
	iouThresh  float64
}

// newONNXDetector loads modelPath and prepares the runtime session.
func newONNXDetector(modelPath string) (Detector, error) {
	if modelPath == "" {
		return nil, errors.New("vision: EVG_ONNX_MODEL is required for the onnx backend")
	}
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("vision: onnx model %s: %w", modelPath, err)
	}
	if lib := os.Getenv("EVG_ONNXRUNTIME_LIB"); lib != "" {
		ort.SetSharedLibraryPath(lib)
	}
	onnxInitOnce.Do(func() {
		onnxInitErr = ort.InitializeEnvironment()
	})
	if onnxInitErr != nil {
		return nil, fmt.Errorf("vision: init onnxruntime: %w", onnxInitErr)
	}

	shape := ort.NewShape(1, 3, onnxInputSize, onnxInputSize)
	inputData := make([]float32, 3*onnxInputSize*onnxInputSize)
	input, err := ort.NewTensor(shape, inputData)
	if err != nil {
		return nil, fmt.Errorf("vision: allocate onnx input: %w", err)
	}
	// YOLOv8 default export: [1, 4+80 classes, N anchors].
	output, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 84, 8400))
	if err != nil {
		_ = input.Destroy()
		return nil, fmt.Errorf("vision: allocate onnx output: %w", err)
	}
	session, err := ort.NewAdvancedSession(modelPath,
		[]string{"images"}, []string{"output0"},
		[]ort.Value{input}, []ort.Value{output}, nil)
	if err != nil {
		_ = input.Destroy()
		_ = output.Destroy()
		return nil, fmt.Errorf("vision: load onnx model (is onnxruntime installed?): %w", err)
	}
	return &ONNXDetector{
		session:    session,
		input:      input,
		output:     output,
		inputData:  inputData,
		confThresh: 0.25,
		iouThresh:  0.45,
	}, nil
}

// Name implements Detector.
func (d *ONNXDetector) Name() string { return "onnx-yolov8" }

// Detect implements Detector: letterbox to 640x640 RGB NCHW, run, decode
// cx/cy/w/h + class scores, map back through the letterbox and NMS.
func (d *ONNXDetector) Detect(img image.Image) ([]Detection, error) {
	if img == nil {
		return nil, errors.New("vision: nil image")
	}
	scale, padX, padY := letterbox(img, d.inputData)
	if err := d.session.Run(); err != nil {
		return nil, fmt.Errorf("vision: onnx run: %w", err)
	}
	out := d.output.GetData()
	shape := d.output.GetShape()
	if len(shape) != 3 {
		return nil, fmt.Errorf("vision: unexpected onnx output rank %d", len(shape))
	}
	channels, anchors := int(shape[1]), int(shape[2])
	if channels < 5 {
		return nil, fmt.Errorf("vision: onnx output has %d channels", channels)
	}
	b := img.Bounds()
	candidates := make([]Detection, 0, 64)
	for i := 0; i < anchors; i++ {
		cx := float64(out[i])
		cy := float64(out[anchors+i])
		w := float64(out[2*anchors+i])
		h := float64(out[3*anchors+i])
		best, bestIdx := float32(0), -1
		for c := 4; c < channels; c++ {
			if s := out[c*anchors+i]; s > best {
				best, bestIdx = s, c-4
			}
		}
		if best < d.confThresh || bestIdx < 0 {
			continue
		}
		x1 := (cx - w/2 - float64(padX)) / scale
		y1 := (cy - h/2 - float64(padY)) / scale
		x2 := (cx + w/2 - float64(padX)) / scale
		y2 := (cy + h/2 - float64(padY)) / scale
		x1 = math.Max(0, math.Min(x1, float64(b.Dx()-1)))
		y1 = math.Max(0, math.Min(y1, float64(b.Dy()-1)))
		x2 = math.Max(0, math.Min(x2, float64(b.Dx())))
		y2 = math.Max(0, math.Min(y2, float64(b.Dy())))
		bw, bh := int(math.Round(x2-x1)), int(math.Round(y2-y1))
		if bw < 1 || bh < 1 {
			continue
		}
		label := "object"
		if bestIdx < len(cocoLabels) {
			label = cocoLabels[bestIdx]
		}
		candidates = append(candidates, Detection{
			Label:      label,
			Confidence: float64(best),
			Box:        Box{X: int(math.Round(x1)), Y: int(math.Round(y1)), W: bw, H: bh},
		})
	}
	return NonMaxSuppression(candidates, d.iouThresh), nil
}

// letterbox resizes img preserving aspect ratio into a 114-grey padded
// 640x640 RGB NCHW buffer and returns the scale and top-left padding offsets.
func letterbox(img image.Image, dst []float32) (scale float64, padX, padY int) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	scale = math.Min(float64(onnxInputSize)/float64(w), float64(onnxInputSize)/float64(h))
	nw := int(math.Round(float64(w) * scale))
	nh := int(math.Round(float64(h) * scale))
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	padX = (onnxInputSize - nw) / 2
	padY = (onnxInputSize - nh) / 2
	plane := onnxInputSize * onnxInputSize
	// Padding colour 114/255, as used by Ultralytics letterboxing.
	const padVal = float32(114.0 / 255.0)
	for i := 0; i < 3*plane; i++ {
		dst[i] = padVal
	}
	for dy := 0; dy < nh; dy++ {
		sy := b.Min.Y + int(float64(dy)/scale)
		if sy >= b.Max.Y {
			sy = b.Max.Y - 1
		}
		for dx := 0; dx < nw; dx++ {
			sx := b.Min.X + int(float64(dx)/scale)
			if sx >= b.Max.X {
				sx = b.Max.X - 1
			}
			r, g, bl, _ := img.At(sx, sy).RGBA()
			idx := (padY+dy)*onnxInputSize + padX + dx
			dst[idx] = float32(r>>8) / 255.0
			dst[plane+idx] = float32(g>>8) / 255.0
			dst[2*plane+idx] = float32(bl>>8) / 255.0
		}
	}
	return scale, padX, padY
}

var _ Detector = (*ONNXDetector)(nil)
