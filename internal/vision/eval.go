package vision

import (
	"sort"
)

// Sample is one frame's predictions paired with its ground truth.
type Sample struct {
	Pred  []Detection
	Truth []Detection
}

// IoU returns the intersection-over-union of two boxes in [0,1]. Boxes with
// zero or negative area yield 0.
func IoU(a, b Box) float64 {
	ax2, ay2 := a.X+a.W, a.Y+a.H
	bx2, by2 := b.X+b.W, b.Y+b.H
	ix1, iy1 := max(a.X, b.X), max(a.Y, b.Y)
	ix2, iy2 := min(ax2, bx2), min(ay2, by2)
	iw, ih := ix2-ix1, iy2-iy1
	if iw <= 0 || ih <= 0 {
		return 0
	}
	inter := float64(iw * ih)
	union := float64(a.Area() + b.Area() - iw*ih)
	if union <= 0 {
		return 0
	}
	return inter / union
}

// Match greedily pairs predictions with ground-truth boxes by descending
// prediction confidence, counting a prediction as a true positive when its best
// unmatched IoU is at least iouThresh.
func Match(pred, truth []Detection, iouThresh float64) (tp, fp, fn int) {
	tp, fp, fn, _ = MatchWithIOU(pred, truth, iouThresh)
	return tp, fp, fn
}

// MatchWithIOU is Match plus the IoU of every matched pair.
func MatchWithIOU(pred, truth []Detection, iouThresh float64) (tp, fp, fn int, matchedIOUs []float64) {
	if len(truth) == 0 {
		return 0, len(pred), 0, nil
	}
	preds := make([]Detection, len(pred))
	copy(preds, pred)
	sort.SliceStable(preds, func(i, j int) bool { return preds[i].Confidence > preds[j].Confidence })
	used := make([]bool, len(truth))
	for _, p := range preds {
		best, bestIoU := -1, 0.0
		for j, t := range truth {
			if used[j] {
				continue
			}
			if v := IoU(p.Box, t.Box); v > bestIoU {
				best, bestIoU = j, v
			}
		}
		if best >= 0 && bestIoU >= iouThresh {
			used[best] = true
			matchedIOUs = append(matchedIOUs, bestIoU)
			tp++
		} else {
			fp++
		}
	}
	for _, u := range used {
		if !u {
			fn++
		}
	}
	return tp, fp, fn, matchedIOUs
}

// AveragePrecision computes all-point interpolated AP over a set of frames.
// Predictions are pooled globally, sorted by confidence, and matched greedily
// per frame; precision is then interpolated as the maximum precision at any
// recall >= r, and AP is the area under that curve. This is the VOC2010+
// (all-point) definition; docs/VISION.md states the same.
func AveragePrecision(all []Sample, iouThresh float64) float64 {
	type scored struct {
		conf float64
		tp   bool
	}
	var scoredDetections []scored
	totalTruth := 0
	for _, s := range all {
		totalTruth += len(s.Truth)
		preds := make([]Detection, len(s.Pred))
		copy(preds, s.Pred)
		sort.SliceStable(preds, func(i, j int) bool { return preds[i].Confidence > preds[j].Confidence })
		used := make([]bool, len(s.Truth))
		for _, p := range preds {
			best, bestIoU := -1, 0.0
			for j, t := range s.Truth {
				if used[j] {
					continue
				}
				if v := IoU(p.Box, t.Box); v > bestIoU {
					best, bestIoU = j, v
				}
			}
			hit := best >= 0 && bestIoU >= iouThresh
			if hit {
				used[best] = true
			}
			scoredDetections = append(scoredDetections, scored{conf: p.Confidence, tp: hit})
		}
	}
	if totalTruth == 0 || len(scoredDetections) == 0 {
		return 0
	}
	sort.SliceStable(scoredDetections, func(i, j int) bool {
		return scoredDetections[i].conf > scoredDetections[j].conf
	})
	precisions := make([]float64, len(scoredDetections))
	recalls := make([]float64, len(scoredDetections))
	tp, fp := 0, 0
	for i, d := range scoredDetections {
		if d.tp {
			tp++
		} else {
			fp++
		}
		precisions[i] = float64(tp) / float64(tp+fp)
		recalls[i] = float64(tp) / float64(totalTruth)
	}
	// Interpolate precision to the right (max precision at recall >= r).
	for i := len(precisions) - 2; i >= 0; i-- {
		if precisions[i+1] > precisions[i] {
			precisions[i] = precisions[i+1]
		}
	}
	ap := 0.0
	prevRecall := 0.0
	for i := range recalls {
		if recalls[i] > prevRecall {
			ap += (recalls[i] - prevRecall) * precisions[i]
			prevRecall = recalls[i]
		}
	}
	return ap
}
