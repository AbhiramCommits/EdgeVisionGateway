package vision

import (
	"encoding/binary"
	"errors"
	"image"
	"image/draw"
	"math"
	"sort"
)

// MotionDetector is a pure-Go background-subtraction detector. The pipeline is:
//
//	ToGray → selective UpdateBackground (running average) → AbsDiffThreshold →
//	open (Erode→Dilate) → close (Dilate→Erode) → ConnectedComponents →
//	area/class filters → NMS
//
// All intermediate images are reused across frames, so steady-state Detect
// allocates only the detection slice. The detector keeps one background model
// and must not be used concurrently; the worker pool creates one instance per
// worker.
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

	bg     *image.Gray
	cur    *image.Gray
	mask   *image.Gray
	tmp1   *image.Gray
	tmp2   *image.Gray
	tmp3   *image.Gray
	labels []int32
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
	b := img.Bounds()
	if b.Dx() < 8 || b.Dy() < 8 {
		return nil, errors.New("vision: frame smaller than 8x8")
	}
	d.ensureBuffers(b)
	toGrayInto(d.cur, img)
	if d.bg == nil {
		d.bg = image.NewGray(d.cur.Bounds())
		copy(d.bg.Pix, d.cur.Pix)
		return []Detection{}, nil
	}

	absDiffInto(d.mask, d.bg, d.cur, d.Threshold)
	// Opening: erode then dilate.
	morphInto(d.tmp1, d.tmp3, d.mask, true)
	morphInto(d.tmp2, d.tmp3, d.tmp1, false)
	// Closing: dilate then erode. tmp2 (opened) is the source for the first
	// step; the result lands back in tmp2 for classification.
	morphInto(d.tmp1, d.tmp3, d.tmp2, false)
	morphInto(d.tmp2, d.tmp3, d.tmp1, true)
	closed := d.tmp2

	boxes := connectedComponentsInto(closed, d.labels)
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

	UpdateBackgroundMasked(d.bg, d.cur, d.mask, d.Alpha)
	return dets, nil
}

func (d *MotionDetector) ensureBuffers(b image.Rectangle) {
	rect := image.Rect(0, 0, b.Dx(), b.Dy())
	if d.cur != nil && d.cur.Bounds() == rect {
		return
	}
	d.cur = image.NewGray(rect)
	d.mask = image.NewGray(rect)
	d.tmp1 = image.NewGray(rect)
	d.tmp2 = image.NewGray(rect)
	d.tmp3 = image.NewGray(rect)
	d.labels = make([]int32, rect.Dx()*rect.Dy())
	if d.bg != nil && d.bg.Bounds() != rect {
		d.bg = nil
	}
}

// ToGray converts any image to an 8-bit grayscale image with origin (0,0).
func ToGray(img image.Image) *image.Gray {
	b := img.Bounds()
	out := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	return toGrayInto(out, img)
}

// toGrayInto fills dst from img. JPEG decodes to *image.YCbCr, whose Y plane is
// already grayscale: copying it avoids a per-pixel colour conversion on the
// gateway's hot path.
func toGrayInto(dst *image.Gray, img image.Image) *image.Gray {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	switch src := img.(type) {
	case *image.Gray:
		if src.Bounds() == dst.Bounds() {
			copy(dst.Pix, src.Pix)
			return dst
		}
		for y := 0; y < h; y++ {
			copy(dst.Pix[y*dst.Stride:y*dst.Stride+w], src.Pix[src.PixOffset(b.Min.X, b.Min.Y+y):][:w])
		}
		return dst
	case *image.YCbCr:
		for y := 0; y < h; y++ {
			off := src.YOffset(b.Min.X, b.Min.Y+y)
			copy(dst.Pix[y*dst.Stride:y*dst.Stride+w], src.Y[off:off+w])
		}
		return dst
	case *image.RGBA:
		for y := 0; y < h; y++ {
			srow := (b.Min.Y + y - src.Rect.Min.Y) * src.Stride
			drow := y * dst.Stride
			for x := 0; x < w; x++ {
				i := srow + (b.Min.X+x-src.Rect.Min.X)*4
				// Integer luma approximation of ITU-R BT.601.
				dst.Pix[drow+x] = uint8((299*uint32(src.Pix[i]) + 587*uint32(src.Pix[i+1]) + 114*uint32(src.Pix[i+2])) / 1000)
			}
		}
		return dst
	default:
		draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
		return dst
	}
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
	// Fixed-point update: k/256 approximates alpha, avoiding two float
	// conversions per pixel on the hot path.
	k := uint32(alpha*256 + 0.5)
	if k > 256 {
		k = 256
	}
	keep := 256 - k
	for i := range bg.Pix {
		if mask.Pix[i] != 0 {
			continue
		}
		bg.Pix[i] = uint8((uint32(bg.Pix[i])*keep + uint32(cur.Pix[i])*k + 128) >> 8)
	}
}

// AbsDiffThreshold returns a binary mask (255 where |bg-cur| > thresh).
func AbsDiffThreshold(bg, cur *image.Gray, thresh uint8) *image.Gray {
	out := image.NewGray(bg.Bounds())
	return absDiffInto(out, bg, cur, thresh)
}

func absDiffInto(out, bg, cur *image.Gray, thresh uint8) *image.Gray {
	for i := range bg.Pix {
		a := int(bg.Pix[i])
		c := int(cur.Pix[i])
		diff := a - c
		if diff < 0 {
			diff = -diff
		}
		if diff > int(thresh) {
			out.Pix[i] = 255
		} else {
			out.Pix[i] = 0
		}
	}
	return out
}

// Erode applies a 3x3 square structuring element: a pixel survives only if all
// 3x3 neighbours (out-of-bounds treated as 0) are set. Masks are binary
// (0 or 255).
func Erode(mask *image.Gray) *image.Gray {
	dst := image.NewGray(mask.Bounds())
	tmp := image.NewGray(mask.Bounds())
	return morphInto(dst, tmp, mask, true)
}

// ErodeInto writes the 3x3 erosion of src into dst.
func ErodeInto(dst, src *image.Gray) *image.Gray {
	return morphInto(dst, image.NewGray(src.Bounds()), src, true)
}

// Dilate applies a 3x3 square structuring element: a pixel is set if any 3x3
// neighbour is set. Masks are binary (0 or 255).
func Dilate(mask *image.Gray) *image.Gray {
	dst := image.NewGray(mask.Bounds())
	tmp := image.NewGray(mask.Bounds())
	return morphInto(dst, tmp, mask, false)
}

// DilateInto writes the 3x3 dilation of src into dst.
func DilateInto(dst, src *image.Gray) *image.Gray {
	return morphInto(dst, image.NewGray(src.Bounds()), src, false)
}

// morphInto is a separable 3x3 min/max filter for binary masks: a horizontal
// pass into tmp, then a vertical pass into dst. Eight columns are processed per
// 64-bit word, so a 640x480 mask costs ~76k word operations instead of 2.7M
// byte comparisons. dst, tmp and src must be distinct buffers.
func morphInto(dst, tmp, src *image.Gray, erode bool) *image.Gray {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	if w < 3 || h < 3 || src.Stride != w || dst.Stride != w || tmp.Stride != w ||
		dst.Bounds().Dx() != w || dst.Bounds().Dy() != h || tmp.Bounds().Dx() != w || tmp.Bounds().Dy() != h {
		return morphGeneric(dst, src, erode)
	}
	// Horizontal pass.
	for y := 0; y < h; y++ {
		srow := src.Pix[y*w : (y+1)*w]
		drow := tmp.Pix[y*w : (y+1)*w]
		if erode {
			drow[0] = 0
			drow[w-1] = 0
		} else {
			drow[0] = srow[0] | srow[1]
			drow[w-1] = srow[w-2] | srow[w-1]
		}
		x := 1
		for ; x+8 <= w-1; x += 8 {
			a := binary.LittleEndian.Uint64(srow[x-1:])
			b := binary.LittleEndian.Uint64(srow[x:])
			c := binary.LittleEndian.Uint64(srow[x+1:])
			var v uint64
			if erode {
				v = a & b & c
			} else {
				v = a | b | c
			}
			binary.LittleEndian.PutUint64(drow[x:], v)
		}
		for ; x < w-1; x++ {
			if erode {
				drow[x] = srow[x-1] & srow[x] & srow[x+1]
			} else {
				drow[x] = srow[x-1] | srow[x] | srow[x+1]
			}
		}
	}
	// Vertical pass.
	zero := make([]byte, w)
	for y := 0; y < h; y++ {
		drow := dst.Pix[y*w : (y+1)*w]
		switch {
		case y == 0 || y == h-1:
			if erode {
				copy(drow, zero)
				continue
			}
			// Only one of the two neighbouring rows exists for dilation.
			other := 1
			if y == h-1 {
				other = y - 1
			}
			brow := tmp.Pix[y*w : (y+1)*w]
			orow := tmp.Pix[other*w : (other+1)*w]
			for x := range drow {
				drow[x] = brow[x] | orow[x]
			}
			continue
		}
		arow := tmp.Pix[(y-1)*w : y*w]
		brow := tmp.Pix[y*w : (y+1)*w]
		crow := tmp.Pix[(y+1)*w : (y+2)*w]
		x := 0
		for ; x+8 <= w; x += 8 {
			a := binary.LittleEndian.Uint64(arow[x:])
			b := binary.LittleEndian.Uint64(brow[x:])
			c := binary.LittleEndian.Uint64(crow[x:])
			var v uint64
			if erode {
				v = a & b & c
			} else {
				v = a | b | c
			}
			binary.LittleEndian.PutUint64(drow[x:], v)
		}
		for ; x < w; x++ {
			if erode {
				drow[x] = arow[x] & brow[x] & crow[x]
			} else {
				drow[x] = arow[x] | brow[x] | crow[x]
			}
		}
	}
	return dst
}

// morphGeneric is the byte-at-a-time fallback for unusual strides or tiny
// frames. It keeps the same 3x3 min/max semantics.
func morphGeneric(dst, src *image.Gray, erode bool) *image.Gray {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	sp := src.Pix
	op := dst.Pix
	stride := src.Stride
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			on := 0
			y0, y1 := y-1, y+1
			x0, x1 := x-1, x+1
			if y0 < 0 {
				y0 = 0
			}
			if y1 >= h {
				y1 = h - 1
			}
			if x0 < 0 {
				x0 = 0
			}
			if x1 >= w {
				x1 = w - 1
			}
			for yy := y0; yy <= y1; yy++ {
				row := yy * stride
				for xx := x0; xx <= x1; xx++ {
					if sp[row+xx] != 0 {
						on++
					}
				}
			}
			set := false
			if erode {
				set = on == 9
			} else if on > 0 {
				set = true
			}
			if set {
				op[y*stride+x] = 255
			} else {
				op[y*stride+x] = 0
			}
		}
	}
	return dst
}

// ConnectedComponents labels 8-connected foreground pixels with a two-pass
// union-find algorithm and returns one tight bounding box per component.
func ConnectedComponents(mask *image.Gray) []Box {
	labels := make([]int32, mask.Bounds().Dx()*mask.Bounds().Dy())
	return connectedComponentsInto(mask, labels)
}

func connectedComponentsInto(mask *image.Gray, labels []int32) []Box {
	w, h := mask.Bounds().Dx(), mask.Bounds().Dy()
	for i := range labels {
		labels[i] = 0
	}
	parent := make([]int32, 1, 1024) // parent[0] is the background sentinel
	next := int32(1)

	find := func(x int32) int32 {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int32) {
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
			var nb [4]int32
			n := 0
			if x > 0 {
				if l := labels[y*w+x-1]; l != 0 {
					nb[n] = l
					n++
				}
			}
			if y > 0 {
				if u := labels[(y-1)*w+x]; u != 0 {
					nb[n] = u
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

	minX := make([]int32, next)
	minY := make([]int32, next)
	maxX := make([]int32, next)
	maxY := make([]int32, next)
	for i := range minX {
		minX[i] = math.MaxInt32
		minY[i] = math.MaxInt32
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			l := labels[y*w+x]
			if l == 0 {
				continue
			}
			root := find(l)
			labels[y*w+x] = root
			if int32(x) < minX[root] {
				minX[root] = int32(x)
			}
			if int32(x) > maxX[root] {
				maxX[root] = int32(x)
			}
			if int32(y) < minY[root] {
				minY[root] = int32(y)
			}
			if int32(y) > maxY[root] {
				maxY[root] = int32(y)
			}
		}
	}
	out := make([]Box, 0, 16)
	for l := int32(1); l < next; l++ {
		if minX[l] == math.MaxInt32 {
			continue
		}
		out = append(out, Box{X: int(minX[l]), Y: int(minY[l]), W: int(maxX[l]-minX[l]) + 1, H: int(maxY[l]-minY[l]) + 1})
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
