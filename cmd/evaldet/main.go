// Package main is the detector accuracy harness. It generates a synthetic video
// sequence with exact ground truth, runs the configured detector over every
// frame, and reports precision, recall, F1, mean IoU, AP@0.5 and per-frame
// detection latency.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/vision"
)

// Report is the JSON artefact written to docs/results/vision_eval.json.
type Report struct {
	Detector       string  `json:"detector"`
	Frames         int     `json:"frames_evaluated"`
	Objects        int     `json:"objects_per_frame"`
	NoiseSigma     float64 `json:"noise_sigma"`
	Seed           int64   `json:"seed"`
	IOUThreshold   float64 `json:"iou_threshold"`
	TruePositives  int     `json:"tp"`
	FalsePositives int     `json:"fp"`
	FalseNegatives int     `json:"fn"`
	Precision      float64 `json:"precision"`
	Recall         float64 `json:"recall"`
	F1             float64 `json:"f1"`
	MeanIOU        float64 `json:"mean_iou"`
	AP50           float64 `json:"ap50"`
	MeanDetectUS   float64 `json:"mean_detect_us"`
	TotalDetectMS  float64 `json:"total_detect_ms"`
}

func main() {
	frames := flag.Int("frames", 300, "number of frames to generate and evaluate")
	objects := flag.Int("objects", 4, "objects per frame")
	noise := flag.Float64("noise", 6, "Gaussian noise sigma per pixel (0-255 scale)")
	seed := flag.Int64("seed", 7, "synthetic scene seed")
	iouThresh := flag.Float64("iou", 0.5, "IoU threshold for a true positive")
	width := flag.Int("width", 640, "frame width")
	height := flag.Int("height", 480, "frame height")
	out := flag.String("out", "docs/results/vision_eval.json", "JSON report path (empty to skip writing)")
	flag.Parse()

	imgs, truth := vision.GenerateSequence(*seed, *frames, *width, *height, *objects, *noise)
	det := vision.NewMotionDetector()

	samples := make([]vision.Sample, 0, len(imgs))
	var tp, fp, fn int
	var iouSum float64
	var detectTotal time.Duration

	for i, img := range imgs {
		start := time.Now()
		preds, err := det.Detect(img)
		elapsed := time.Since(start)
		detectTotal += elapsed
		if err != nil {
			fmt.Fprintf(os.Stderr, "frame %d: %v\n", i, err)
			os.Exit(1)
		}
		samples = append(samples, vision.Sample{Pred: preds, Truth: truth[i]})
		ft, ff, fmiss, ious := vision.MatchWithIOU(preds, truth[i], *iouThresh)
		tp += ft
		fp += ff
		fn += fmiss
		for _, v := range ious {
			iouSum += v
		}
	}

	precision, recall, f1 := 0.0, 0.0, 0.0
	if tp+fp > 0 {
		precision = float64(tp) / float64(tp+fp)
	}
	if tp+fn > 0 {
		recall = float64(tp) / float64(tp+fn)
	}
	if precision+recall > 0 {
		f1 = 2 * precision * recall / (precision + recall)
	}
	meanIOU := 0.0
	if tp > 0 {
		meanIOU = iouSum / float64(tp)
	}
	meanUS := 0.0
	if len(imgs) > 0 {
		meanUS = float64(detectTotal.Microseconds()) / float64(len(imgs))
	}

	report := Report{
		Detector:       det.Name(),
		Frames:         len(imgs),
		Objects:        *objects,
		NoiseSigma:     *noise,
		Seed:           *seed,
		IOUThreshold:   *iouThresh,
		TruePositives:  tp,
		FalsePositives: fp,
		FalseNegatives: fn,
		Precision:      precision,
		Recall:         recall,
		F1:             f1,
		MeanIOU:        meanIOU,
		AP50:           vision.AveragePrecision(samples, *iouThresh),
		MeanDetectUS:   meanUS,
		TotalDetectMS:  float64(detectTotal.Microseconds()) / 1000,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		os.Exit(1)
	}

	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil && filepath.Dir(*out) != "." {
			fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
			os.Exit(1)
		}
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
	}
}
