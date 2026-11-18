package vision

import (
	"image"
	"image/color"
	"math"
	"math/rand"
)

// synthWarmupFrames is the number of empty frames at the start of a generated
// sequence. They let a background-subtraction detector learn the static scene
// before objects appear, exactly as a real camera would before anyone walks in.
const synthWarmupFrames = 12

// GenerateSequence builds a deterministic synthetic video sequence and the exact
// ground truth for every frame.
//
// The scene is a static gradient background plus nObjects filled rectangles
// (alternating "person" and "object" aspect ratios) that move horizontally in
// disjoint lanes, bouncing off the sides, with a slow vertical bob. Gaussian
// noise of standard deviation noiseSigma is added to every pixel. The first
// synthWarmupFrames frames contain no objects and carry empty ground truth.
//
// The returned images and detections have the requested frame count.
func GenerateSequence(seed int64, frames, width, height int, nObjects int, noiseSigma float64) ([]image.Image, [][]Detection) {
	if frames < 0 {
		frames = 0
	}
	if width < 32 {
		width = 32
	}
	if height < 32 {
		height = 32
	}
	if nObjects < 0 {
		nObjects = 0
	}
	if nObjects > height/16 {
		nObjects = height / 16
	}

	rng := rand.New(rand.NewSource(seed))
	margin := 8
	usableH := height - 2*margin
	laneH := usableH
	if nObjects > 0 {
		laneH = usableH / nObjects
	}

	type obj struct {
		w, h      int
		x, y      int
		vx        int
		intensity uint8
		label     string
		laneTop   int
		phase     float64
	}
	objs := make([]obj, 0, nObjects)
	for i := 0; i < nObjects; i++ {
		laneTop := margin + i*laneH
		person := i%2 == 0
		var h, w int
		if person {
			h = min(72, laneH*3/4)
			w = max(20, h/2)
		} else {
			h = min(44, laneH/2)
			w = max(30, h*3/2)
		}
		if w >= width-2*margin {
			w = width - 2*margin - 1
		}
		if h > laneH-2 {
			h = laneH - 2
		}
		startX := margin + (i*37)%max(1, width-2*margin-w)
		intensity := uint8(30)
		label := "person"
		if !person {
			intensity = 248
			label = "object"
		}
		objs = append(objs, obj{
			w: w, h: h,
			x:         startX,
			y:         laneTop + (laneH-h)/2,
			vx:        3 + i%3,
			intensity: intensity,
			label:     label,
			laneTop:   laneTop,
			phase:     float64(i) * 1.7,
		})
	}

	images := make([]image.Image, frames)
	truths := make([][]Detection, frames)

	// The static gradient is generated once; each frame copies it.
	base := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		gy := 30 * y / height
		for x := 0; x < width; x++ {
			v := uint8(100 + 50*x/width + gy)
			i := base.PixOffset(x, y)
			base.Pix[i] = v
			base.Pix[i+1] = v
			base.Pix[i+2] = v
			base.Pix[i+3] = 255
		}
	}

	for f := 0; f < frames; f++ {
		img := image.NewRGBA(base.Bounds())
		copy(img.Pix, base.Pix)

		var truth []Detection
		if f >= synthWarmupFrames {
			truth = make([]Detection, 0, len(objs))
		}

		for i := range objs {
			o := &objs[i]
			if f >= synthWarmupFrames {
				// Horizontal bounce.
				o.x += o.vx
				if o.x <= margin || o.x+o.w >= width-margin {
					o.x -= 2 * o.vx
					o.vx = -o.vx
				}
				bob := int(math.Round(4 * math.Sin(float64(f)/9+o.phase)))
				center := o.laneTop + laneH/2
				o.y = center - o.h/2 + bob
				if o.y < o.laneTop+1 {
					o.y = o.laneTop + 1
				}
				if o.y+o.h > o.laneTop+laneH-1 {
					o.y = o.laneTop + laneH - 1 - o.h
				}
				for y := o.y; y < o.y+o.h; y++ {
					for x := o.x; x < o.x+o.w; x++ {
						j := img.PixOffset(x, y)
						img.Pix[j] = o.intensity
						img.Pix[j+1] = o.intensity
						img.Pix[j+2] = o.intensity
					}
				}
				truth = append(truth, Detection{
					Label:      o.label,
					Confidence: 1,
					Box:        Box{X: o.x, Y: o.y, W: o.w, H: o.h},
				})
			}
		}

		if noiseSigma > 0 {
			for i := 0; i < len(img.Pix); i += 4 {
				d := rng.NormFloat64() * noiseSigma
				for c := 0; c < 3; c++ {
					v := float64(img.Pix[i+c]) + d
					switch {
					case v < 0:
						img.Pix[i+c] = 0
					case v > 255:
						img.Pix[i+c] = 255
					default:
						img.Pix[i+c] = uint8(v)
					}
				}
			}
		}
		images[f] = img
		if truth == nil {
			truth = []Detection{}
		}
		truths[f] = truth
	}
	return images, truths
}

// DrawBox overlays a 1px white rectangle, used for debugging snapshots.
func DrawBox(img *image.RGBA, b Box) {
	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	r := image.Rect(b.X, b.Y, b.X+b.W, b.Y+b.H)
	for x := r.Min.X; x < r.Max.X; x++ {
		if image.Pt(x, r.Min.Y).In(img.Bounds()) {
			img.Set(x, r.Min.Y, white)
			img.Set(x, r.Max.Y-1, white)
		}
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		if image.Pt(r.Min.X, y).In(img.Bounds()) {
			img.Set(r.Min.X, y, white)
			img.Set(r.Max.X-1, y, white)
		}
	}
}
