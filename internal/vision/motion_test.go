package vision

import (
	"image"
	"image/color"
	"testing"
)

func maskFromPoints(w, h int, pts ...[2]int) *image.Gray {
	m := image.NewGray(image.Rect(0, 0, w, h))
	for _, p := range pts {
		m.SetGray(p[0], p[1], color.Gray{Y: 255})
	}
	return m
}

func fillRect(m *image.Gray, x0, y0, w, h int) {
	for y := y0; y < y0+h; y++ {
		for x := x0; x < x0+w; x++ {
			m.SetGray(x, y, color.Gray{Y: 255})
		}
	}
}

func TestToGray(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	img.Set(1, 0, color.RGBA{})
	g := ToGray(img)
	if got := g.GrayAt(0, 0).Y; got < 250 {
		t.Errorf("white pixel = %d, want ~255", got)
	}
	if got := g.GrayAt(1, 0).Y; got != 0 {
		t.Errorf("black pixel = %d, want 0", got)
	}
}

func TestUpdateBackgroundRunningAverage(t *testing.T) {
	bg := image.NewGray(image.Rect(0, 0, 1, 1))
	cur := image.NewGray(image.Rect(0, 0, 1, 1))
	cur.Pix[0] = 100
	UpdateBackground(bg, cur, 0.5)
	if got := bg.Pix[0]; got != 50 {
		t.Errorf("bg after alpha 0.5 = %d, want 50", got)
	}
	UpdateBackground(bg, cur, 0.5)
	if got := bg.Pix[0]; got != 75 {
		t.Errorf("bg after second update = %d, want 75", got)
	}
}

func TestUpdateBackgroundMaskedFreezesForeground(t *testing.T) {
	bg := image.NewGray(image.Rect(0, 0, 3, 1))
	cur := image.NewGray(image.Rect(0, 0, 3, 1))
	for i := range cur.Pix {
		cur.Pix[i] = 200
	}
	mask := maskFromPoints(3, 1, [2]int{1, 0})
	UpdateBackgroundMasked(bg, cur, mask, 1.0)
	if bg.Pix[0] != 200 || bg.Pix[2] != 200 {
		t.Errorf("unmasked pixels should learn: %v", bg.Pix)
	}
	if bg.Pix[1] != 0 {
		t.Errorf("masked pixel learned (bg=%d), want frozen at 0", bg.Pix[1])
	}
}

func TestAbsDiffThreshold(t *testing.T) {
	bg := image.NewGray(image.Rect(0, 0, 4, 1))
	cur := image.NewGray(image.Rect(0, 0, 4, 1))
	copy(bg.Pix, []uint8{100, 100, 100, 100})
	copy(cur.Pix, []uint8{100, 120, 131, 69})
	out := AbsDiffThreshold(bg, cur, 30)
	want := []uint8{0, 0, 255, 255}
	for i, w := range want {
		if out.Pix[i] != w {
			t.Errorf("mask[%d] = %d, want %d", i, out.Pix[i], w)
		}
	}
}

func TestErodeAndDilate(t *testing.T) {
	m := image.NewGray(image.Rect(0, 0, 7, 7))
	fillRect(m, 1, 1, 5, 5)
	e := Erode(m)
	// 5x5 solid becomes 3x3 solid.
	if e.Pix[2*7+2] != 255 {
		t.Error("erode removed interior")
	}
	if e.Pix[1*7+1] != 0 || e.Pix[1*7+2] != 0 {
		t.Error("erode did not trim border")
	}
	if e.Pix[3*7+3] != 255 {
		t.Error("erode over-trimmed")
	}
	d := Dilate(maskFromPoints(7, 7, [2]int{3, 3}))
	count := 0
	for _, v := range d.Pix {
		if v != 0 {
			count++
		}
	}
	if count != 9 {
		t.Errorf("dilate single pixel produced %d pixels, want 9", count)
	}
}

func TestConnectedComponentsSeparateBlobs(t *testing.T) {
	m := image.NewGray(image.Rect(0, 0, 20, 20))
	fillRect(m, 2, 2, 4, 4)
	fillRect(m, 10, 10, 3, 3)
	boxes := ConnectedComponents(m)
	if len(boxes) != 2 {
		t.Fatalf("got %d components, want 2: %+v", len(boxes), boxes)
	}
	if boxes[0] != (Box{X: 2, Y: 2, W: 4, H: 4}) {
		t.Errorf("box[0] = %+v", boxes[0])
	}
	if boxes[1] != (Box{X: 10, Y: 10, W: 3, H: 3}) {
		t.Errorf("box[1] = %+v", boxes[1])
	}
}

func TestConnectedComponentsDiagonalIsEightConnected(t *testing.T) {
	m := maskFromPoints(10, 10, [2]int{1, 1}, [2]int{2, 2}, [2]int{3, 3})
	boxes := ConnectedComponents(m)
	if len(boxes) != 1 {
		t.Fatalf("diagonal chain split into %d components: %+v", len(boxes), boxes)
	}
	if boxes[0] != (Box{X: 1, Y: 1, W: 3, H: 3}) {
		t.Errorf("box = %+v, want {1 1 3 3}", boxes[0])
	}
}

func TestConnectedComponentsEmpty(t *testing.T) {
	if got := ConnectedComponents(image.NewGray(image.Rect(0, 0, 8, 8))); len(got) != 0 {
		t.Errorf("empty mask produced %v", got)
	}
}

func TestClassifyBox(t *testing.T) {
	m := image.NewGray(image.Rect(0, 0, 60, 60))
	fillRect(m, 5, 5, 20, 40)
	label, conf := ClassifyBox(Box{X: 5, Y: 5, W: 20, H: 40}, m)
	if label != "person" {
		t.Errorf("tall filled box label = %q, want person", label)
	}
	if conf <= 0 || conf > 1 {
		t.Errorf("conf = %v, want (0,1]", conf)
	}
	fillRect(m, 30, 20, 25, 10)
	label, _ = ClassifyBox(Box{X: 30, Y: 20, W: 25, H: 10}, m)
	if label != "object" {
		t.Errorf("wide box label = %q, want object", label)
	}
}

func TestNonMaxSuppression(t *testing.T) {
	dets := []Detection{
		{Label: "person", Confidence: 0.9, Box: Box{X: 0, Y: 0, W: 10, H: 10}},
		{Label: "person", Confidence: 0.8, Box: Box{X: 1, Y: 0, W: 10, H: 10}},
		{Label: "object", Confidence: 0.7, Box: Box{X: 50, Y: 50, W: 10, H: 10}},
	}
	got := NonMaxSuppression(dets, 0.4)
	if len(got) != 2 {
		t.Fatalf("kept %d boxes, want 2: %+v", len(got), got)
	}
	if got[0].Confidence != 0.9 || got[1].Confidence != 0.7 {
		t.Errorf("wrong boxes kept: %+v", got)
	}
}

func TestDetectFirstFrameInitialisesBackground(t *testing.T) {
	d := NewMotionDetector()
	dets, err := d.Detect(image.NewRGBA(image.Rect(0, 0, 64, 48)))
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(dets) != 0 {
		t.Errorf("first frame returned %d detections, want 0", len(dets))
	}
	if d.Name() != "motion-v1" {
		t.Errorf("name = %q", d.Name())
	}
}

func TestDetectRejectsTinyFrame(t *testing.T) {
	d := NewMotionDetector()
	if _, err := d.Detect(image.NewRGBA(image.Rect(0, 0, 4, 4))); err == nil {
		t.Fatal("want error for tiny frame")
	}
	if _, err := d.Detect(nil); err == nil {
		t.Fatal("want error for nil frame")
	}
}

// TestEndToEndRecall runs the full pipeline on a synthetic sequence and asserts
// the recall measured for this exact configuration (1.000 on this build) with
// headroom, so the test catches regressions without being flaky.
func TestEndToEndRecall(t *testing.T) {
	imgs, truths := GenerateSequence(7, 60, 320, 240, 4, 6)
	d := NewMotionDetector()
	var tp, fp, fn int
	for i, img := range imgs {
		preds, err := d.Detect(img)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		a, b, c := Match(preds, truths[i], 0.5)
		tp, fp, fn = tp+a, fp+b, fn+c
	}
	recall := float64(tp) / float64(tp+fn)
	precision := float64(tp) / float64(tp+fp)
	if recall < 0.95 {
		t.Errorf("recall = %.3f, want >= 0.95", recall)
	}
	if precision < 0.90 {
		t.Errorf("precision = %.3f, want >= 0.90", precision)
	}
}
