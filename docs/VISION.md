# Computer vision pipeline

The gateway ships a pluggable `vision.Detector` interface:

```go
type Detector interface {
    Detect(img image.Image) ([]Detection, error)
    Name() string
}
```

Two backends implement it:

| Backend | Build | Runtime dependencies | Use |
| --- | --- | --- | --- |
| `MotionDetector` (`motion-v1`) | default | none (pure Go) | default, benchmarks, CI |
| `ONNXDetector` (`onnx-yolov8`) | `-tags onnx` | ONNX Runtime shared library + YOLO model | optional production path |

`vision.FromConfig(backend, modelPath)` selects `onnx` when requested and
**falls back to `MotionDetector` with a `slog.Warn`** if the backend was not
compiled in or the model/library cannot be loaded, so a misconfigured
deployment still ingests frames.

## MotionDetector: stages

Each stage is a separately tested function in `internal/vision/motion.go`.

1. **`ToGray`** — converts any `image.Image` to an 8-bit gray image with origin
   (0,0). JPEG/PNG decoding happens in the worker via `image/jpeg` and
   `image/png`.
2. **`UpdateBackgroundMasked(bg, cur, mask, alpha)`** — selective running
   average, `bg = (1-alpha)*bg + alpha*cur` on pixels that are *not* foreground
   (`mask == 0`). Default `alpha = 0.02`. Freezing foreground pixels is what
   prevents a moving object from being absorbed into the background and leaving
   a trailing ghost. The plain, unmasked `UpdateBackground` is also exported and
   tested as the textbook running-average model.
3. **`AbsDiffThreshold`** — binary mask where `|bg-cur| > 30` (0-255 scale).
4. **Morphology** — 3x3 square structuring element: opening
   (`Erode`→`Dilate`) removes salt-and-pepper noise, then closing
   (`Dilate`→`Erode`) reconnects object edges.
5. **`ConnectedComponents`** — 8-connected two-pass union-find labelling with
   path compression and one tight bounding box per component.
6. **Filters** — minimum area 250 px, minimum width/height 4 px.
7. **`ClassifyBox`** — aspect ratio `h/w >= 1.3` and fill ratio `> 0.45`
   classifies `person`; otherwise `object`. Confidence is
   `fill_ratio * min(1, area/4096)` clamped to [0,1].
8. **`NonMaxSuppression`** — greedy by confidence, suppress IoU > 0.4.

The detector keeps one background model and therefore must not be used
concurrently; the worker pool creates one detector per worker.

Known limitation of the selective model: an object that stops moving stays
"foreground" indefinitely (an occupancy signal), and a frame where the scene is
already populated at initialisation cannot bootstrap a clean background. The
synthetic generator models a camera whose scene is empty for the first 12
frames, which is the normal background-subtraction bootstrap.

## Accuracy methodology

`internal/vision/eval.go`:

- **`IoU(a,b)`** — intersection over union of two axis-aligned boxes.
- **`Match(pred, truth, iouThresh)`** — predictions are sorted by descending
  confidence; each is greedily matched to the unmatched ground-truth box with
  the highest IoU. A match at `IoU >= threshold` is a TP, any other prediction
  is an FP, and any unmatched truth is an FN. Matching is **label-agnostic**
  (boxes only); labels are still produced and used by the API.
- **`AveragePrecision(samples, iouThresh)`** — **all-point interpolated AP**
  (VOC2010+ style): predictions from all frames are pooled and sorted by
  confidence, precision is right-interpolated to the maximum precision at any
  recall ≥ r, and AP is the area under that curve.

The synthetic generator (`synth.go`) produces a gradient background, 4 moving
filled rectangles (alternating tall/dark `person` and wide/bright `object`) in
disjoint lanes, Gaussian noise per pixel, and exact ground-truth boxes. It is
deterministic for a given seed. Frames 0-11 are empty warm-up frames.

### Measured results

Standard run (`go run ./cmd/evaldet -frames 300 -objects 4 -noise 6 -seed 7`):

| Metric | Value |
| --- | ---: |
| Frames evaluated | 300 |
| TP / FP / FN | 1152 / 0 / 0 |
| Precision | 1.0000 |
| Recall | 1.0000 |
| F1 | 1.0000 |
| Mean IoU (matched) | 1.0000 |
| AP@0.5 | 1.0000 |
| Mean detect latency | 12,269 µs/frame @ 640x480 |

The perfect score is real for this scene, but it is also the expected outcome:
high-contrast filled rectangles on a static background are exactly the input a
background-subtraction detector is designed for. It is **not** a claim about
real-world imagery. The noise sweep (`docs/results/vision_eval_sweep.md`) is
the more informative measurement: accuracy holds through sigma 32, degrades at
sigma 48 (precision 0.9051, recall 0.9271) and collapses at sigma 64, which is
the honest operating limit of a fixed-threshold background model.

Raw artefacts: `docs/results/vision_eval.json` and
`docs/results/vision_eval_noise*.json`.

## Tests

```sh
go test ./internal/vision/...
```

Covers IoU, greedy matching, all-point AP against a hand-computed value,
morphology, connected components (separate and diagonal blobs), NMS,
classification, synthetic-ground-truth invariants, and an end-to-end recall
assertion (95% floor on a measurement of 100%).

## Building with the ONNX backend

```sh
# 1. Install ONNX Runtime (Homebrew shown; any 1.x shared library works)
brew install onnxruntime

# 2. Build and run with the tag, pointing at the library and a YOLOv8 export
go build -tags onnx -o bin/gateway-onnx ./cmd/gateway
EVG_DETECTOR=onnx \
EVG_ONNX_MODEL=/absolute/path/yolov8n.onnx \
EVG_ONNXRUNTIME_LIB=/opt/homebrew/lib/libonnxruntime.dylib \
./bin/gateway-onnx
```

The ONNX backend (`onnx.go`) expects a YOLOv8-style model: input
`images` `[1,3,640,640]` float32 RGB NCHW, output `output0`
`[1,4+classes,anchors]`. It letterboxes with 114-grey padding, decodes
`cx,cy,w,h` plus per-class scores, maps boxes back through the letterbox, and
runs the same NMS. `EVG_ONNXRUNTIME_LIB` is optional if the library is on the
loader path.

Verification status in this build: `go build -tags onnx ./internal/vision/`
compiles (exit 0), but no ONNX model or runtime shared library was installed,
so the backend was **not executed** and no ONNX accuracy or latency numbers are
claimed. Ingesting with `EVG_DETECTOR=onnx` on a default build logs a warning
and uses `motion-v1`.
