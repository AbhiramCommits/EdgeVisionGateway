package vision

import (
	"errors"
	"image"
	"image/draw"
	"math"
	"sort"
)

// MotionDetector is a pure-Go background-subtraction detector. The pipeline is:
//
//	ToGray → UpdateBackground (running average) → AbsDiffThreshold →
//	open (Erode→Dilate) → close (Dilate→Erode) → ConnectedComponents →
//	area/class filters → NMS
//
// The detector keeps one background model, so one instance must be used per
// camera stream (or per worker). Detect must not be called concurrently on the
// same instance.
type MotionDetector struct {
	// Alpha is the running-average background learning rate, in (0,1].
	Alpha float64
	// Threshold is the per-pixel absolute-difference threshold (0-255).
	Threshold uint8
	// MinArea is the minimum component area in pixels.
	MinArea int
	// MaxArea, when > 0, rejects components larger than this many pixels.
	MaxArea int
	// NMSIoU is the IoU above which overlapping boxes are suppressed.
	NMSIoU float64
	// MinConfidence rejects low-confidence classifications.
	MinConfidence float64

	bg *image.Gray
}

// NewMotionDetector returns a detector tuned for the synthetic scenes in this
// repository (640x480 and 1280x720 frames, objects >= 30x40 px).
func NewMotionDetector() *MotionDetector {
	return &MotionDetector{
		Alpha:         0.02,
		Threshold:     30,
		MinArea:       250,
		MaxArea:       0,
		NMSIoU:        0.4,
		MinConfidence: 0.10,
	}
}

// Name implements Detector.
func (d *MotionDetector) Name() string { return "motion-v1" }

// Reset discards the background model (e.g. after a camera reconnects).
func (d *MotionDetector) Reset() { d.bg = nil }

// Detect implements Detector. The first frame after creation (or Reset) only
// initialises the background model and returns no detections.
func (d *MotionDetector) Detect(img image.Image) ([]Detection, error) {
	if img == nil {
		return nil, errors.New("vision: nil image")
	}
	cur := ToGray(img)
	b := cur.Bounds()
	if b.Dx() < 8 || b.Dy() < 8 {
		return nil, errors.New("vision: frame smaller than 8x8")
	}
	if d.bg == nil || d.bg.Bounds() != b {
		d.bg = cloneGray(cur)
		return []Detection{}, nil
	}

	mask := AbsDiffThreshold(d.bg, cur, d.Threshold)
	// Opening removes salt-and-pepper noise; closing reconnects object edges.
	opened := Dilate(Erode(mask))
	closed := Erode(Dilate(opened))

	boxes := ConnectedComponents(closed)
	dets := make([]Detection, 0, len(boxes))
	for _, box := range boxes {
		area := box.Area()
		if area < d.MinArea {
			continue
		}
		if d.MaxArea > 0 && area > d.MaxArea {
			continue
		}
		if box.W < 4 || box.H < 4 {
			continue
		}
		label, conf := ClassifyBox(box, closed)
		if conf < d.MinConfidence {
			continue
		}
		dets = append(dets, Detection{Label: label, Confidence: conf, Box: box})
	}
	dets = NonMaxSuppression(dets, d.NMSIoU)

	UpdateBackgroundMasked(d.bg, cur, mask, d.Alpha)
	return dets, nil
}

// ToGray converts any image to an 8-bit grayscale image with origin (0,0).
func ToGray(img image.Image) *image.Gray {
	if g, ok := img.(*image.Gray); ok {
		if g.Bounds().Min == (image.Point{}) {
			out := image.NewGray(g.Bounds())
			copy(out.Pix, g.Pix)
			return out
		}
	}
	b := img.Bounds()
	out := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	return out
}

func cloneGray(g *image.Gray) *image.Gray {
	out := image.NewGray(g.Bounds())
	copy(out.Pix, g.Pix)
	return out
}

// UpdateBackground moves bg toward cur by alpha: bg = (1-alpha)*bg + alpha*cur.
// alpha outside (0,1] is clamped.
func UpdateBackground(bg, cur *image.Gray, alpha float64) {
	if bg == nil || cur == nil || bg.Bounds() != cur.Bounds() {
		return
	}
	if alpha <= 0 {
		return
	}
	if alpha > 1 {
		alpha = 1
	}
	keep := 1 - alpha
	for i := range bg.Pix {
		bg.Pix[i] = uint8(keep*float64(bg.Pix[i]) + alpha*float64(cur.Pix[i]) + 0.5)
	}
}

// UpdateBackgroundMasked is the selective running-average update used by
// Detect: pixels marked as foreground in mask are frozen, so a moving object is
// never absorbed into the background model (which is what creates trailing
// "ghost" blobs). Background pixels keep learning at rate alpha, so sensor
// noise and slow illumination drift are still averaged out.
func UpdateBackgroundMasked(bg, cur, mask *image.Gray, alpha float64) {
	if bg == nil || cur == nil || mask == nil {
		return
	}
	if bg.Bounds() != cur.Bounds() || bg.Bounds() != mask.Bounds() {
		return
	}
	if alpha <= 0 {
		return
	}
	if alpha > 1 {
		alpha = 1
	}
	keep := 1 - alpha
	for i := range bg.Pix {
		if mask.Pix[i] != 0 {
			continue
		}
		bg.Pix[i] = uint8(keep*float64(bg.Pix[i]) + alpha*float64(cur.Pix[i]) + 0.5)
	}
}

// AbsDiffThreshold returns a binary mask (255 where |bg-cur| > thresh).
func AbsDiffThreshold(bg, cur *image.Gray, thresh uint8) *image.Gray {
	b := bg.Bounds()
	out := image.NewGray(b)
	for i := range bg.Pix {
		a := int(bg.Pix[i])
		c := int(cur.Pix[i])
		diff := a - c
		if diff < 0 {
			diff = -diff
		}
		if diff > int(thresh) {
			out.Pix[i] = 255
		}
	}
	return out
}

// Erode applies a 3x3 square structuring element: a pixel survives only if all
// 3x3 neighbours (out-of-bounds treated as 0) are set.
func Erode(mask *image.Gray) *image.Gray {
	return morph(mask, true)
}

// Dilate applies a 3x3 square structuring element: a pixel is set if any 3x3
// neighbour is set.
func Dilate(mask *image.Gray) *image.Gray {
	return morph(mask, false)
}

func morph(src *image.Gray, erode bool) *image.Gray {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	out := image.NewGray(image.Rect(0, 0, w, h))
	sp := src.Pix
	op := out.Pix
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			on := 0
			for dy := -1; dy <= 1; dy++ {
				yy := y + dy
				if yy < 0 || yy >= h {
					continue
				}
				row := yy * src.Stride
				for dx := -1; dx <= 1; dx++ {
					xx := x + dx
					if xx < 0 || xx >= w {
						continue
					}
					if sp[row+xx] != 0 {
						on++
					}
				}
			}
			if erode {
				if on == 9 {
					op[y*src.Stride+x] = 255
				}
			} else if on > 0 {
				op[y*src.Stride+x] = 255
			}
		}
	}
	return out
}

// ConnectedComponents labels 8-connected foreground pixels with a two-pass
// union-find algorithm and returns one tight bounding box per component.
func ConnectedComponents(mask *image.Gray) []Box {
	w, h := mask.Bounds().Dx(), mask.Bounds().Dy()
	labels := make([]int, w*h)
	parent := make([]int, 1, 1024) // parent[0] is the background sentinel
	next := 1

	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if ra < rb {
			parent[rb] = ra
		} else {
			parent[ra] = rb
		}
	}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if mask.Pix[y*mask.Stride+x] == 0 {
				continue
			}
			left, up := 0, 0
			var nb [4]int
			n := 0
			if x > 0 {
				left = labels[y*w+x-1]
				if left != 0 {
					nb[n] = left
					n++
				}
			}
			if y > 0 {
				up = labels[(y-1)*w+x]
				if up != 0 {
					nb[n] = up
					n++
				}
				if x > 0 {
					if ul := labels[(y-1)*w+x-1]; ul != 0 {
						nb[n] = ul
						n++
					}
				}
				if x+1 < w {
					if ur := labels[(y-1)*w+x+1]; ur != 0 {
						nb[n] = ur
						n++
					}
				}
			}
			if n == 0 {
				parent = append(parent, next)
				labels[y*w+x] = next
				next++
				continue
			}
			labels[y*w+x] = nb[0]
			for k := 1; k < n; k++ {
				union(nb[0], nb[k])
			}
		}
	}

	type bounds struct{ minX, minY, maxX, maxY int }
	boxes := make(map[int]bounds, next)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			l := labels[y*w+x]
			if l == 0 {
				continue
			}
			root := find(l)
			labels[y*w+x] = root
			bb, ok := boxes[root]
			if !ok {
				boxes[root] = bounds{x, y, x, y}
				continue
			}
			if x < bb.minX {
				bb.minX = x
			}
			if x > bb.maxX {
				bb.maxX = x
			}
			if y < bb.minY {
				bb.minY = y
			}
			if y > bb.maxY {
				bb.maxY = y
			}
			boxes[root] = bb
		}
	}

	out := make([]Box, 0, len(boxes))
	for _, bb := range boxes {
		out = append(out, Box{X: bb.minX, Y: bb.minY, W: bb.maxX - bb.minX + 1, H: bb.maxY - bb.minY + 1})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Y != out[j].Y {
			return out[i].Y < out[j].Y
		}
		return out[i].X < out[j].X
	})
	return out
}

// ClassifyBox maps geometry to a label with an aspect-ratio + fill-ratio
// heuristic: tall boxes with enough fill are people, everything else is an
// object. Confidence is fill × area score, clamped to [0,1].
func ClassifyBox(b Box, mask *image.Gray) (label string, conf float64) {
	if b.W <= 0 || b.H <= 0 {
		return "object", 0
	}
	fg := 0
	mb := mask.Bounds()
	for y := b.Y; y < b.Y+b.H; y++ {
		if y < 0 || y >= mb.Dy() {
			continue
		}
		row := y * mask.Stride
		for x := b.X; x < b.X+b.W; x++ {
			if x < 0 || x >= mb.Dx() {
				continue
			}
			if mask.Pix[row+x] != 0 {
				fg++
			}
		}
	}
	fill := float64(fg) / float64(b.W*b.H)
	aspect := float64(b.H) / float64(b.W)
	label = "object"
	if aspect >= 1.3 && fill > 0.45 {
		label = "person"
	}
	areaScore := math.Min(1, float64(b.Area())/4096.0)
	conf = fill * areaScore
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}
	return label, conf
}

// NonMaxSuppression greedily keeps the highest-confidence box and drops any
// later box whose IoU with a kept box exceeds iouThresh.
func NonMaxSuppression(dets []Detection, iouThresh float64) []Detection {
	if len(dets) < 2 {
		return dets
	}
	sorted := make([]Detection, len(dets))
	copy(sorted, dets)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Confidence > sorted[j].Confidence })
	keep := make([]Detection, 0, len(sorted))
	for _, d := range sorted {
		suppressed := false
		for _, k := range keep {
			if IoU(d.Box, k.Box) > iouThresh {
				suppressed = true
				break
			}
		}
		if !suppressed {
			keep = append(keep, d)
		}
	}
	return keep
}
