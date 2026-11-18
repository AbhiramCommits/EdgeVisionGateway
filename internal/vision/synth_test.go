package vision

import (
	"bytes"
	"image"
	"testing"
)

func TestGenerateSequenceShape(t *testing.T) {
	imgs, truths := GenerateSequence(1, 40, 320, 240, 4, 4)
	if len(imgs) != 40 || len(truths) != 40 {
		t.Fatalf("lengths = %d/%d, want 40/40", len(imgs), len(truths))
	}
	for i := 0; i < synthWarmupFrames; i++ {
		if len(truths[i]) != 0 {
			t.Fatalf("warmup frame %d has %d ground-truth boxes", i, len(truths[i]))
		}
	}
	for i := synthWarmupFrames; i < 40; i++ {
		if len(truths[i]) != 4 {
			t.Fatalf("frame %d has %d ground-truth boxes, want 4", i, len(truths[i]))
		}
		for _, d := range truths[i] {
			b := d.Box
			if b.X < 0 || b.Y < 0 || b.X+b.W > 320 || b.Y+b.H > 240 {
				t.Fatalf("frame %d truth box out of bounds: %+v", i, b)
			}
			if d.Label != "person" && d.Label != "object" {
				t.Fatalf("unexpected label %q", d.Label)
			}
		}
	}
}

func TestGenerateSequenceDeterministic(t *testing.T) {
	a, at := GenerateSequence(42, 20, 160, 120, 2, 8)
	b, bt := GenerateSequence(42, 20, 160, 120, 2, 8)
	for i := range a {
		ra := a[i].(*image.RGBA)
		rb := b[i].(*image.RGBA)
		if !bytes.Equal(ra.Pix, rb.Pix) {
			t.Fatalf("frame %d differs for same seed", i)
		}
		if len(at[i]) != len(bt[i]) {
			t.Fatalf("frame %d truth length differs", i)
		}
	}
	c, _ := GenerateSequence(43, 20, 160, 120, 2, 8)
	if bytes.Equal(a[10].(*image.RGBA).Pix, c[10].(*image.RGBA).Pix) {
		t.Error("different seeds produced identical frames")
	}
}

func TestGenerateSequenceNoiseChangesPixels(t *testing.T) {
	clean, _ := GenerateSequence(5, 1, 64, 64, 0, 0)
	noisy, _ := GenerateSequence(5, 1, 64, 64, 0, 12)
	if bytes.Equal(clean[0].(*image.RGBA).Pix, noisy[0].(*image.RGBA).Pix) {
		t.Error("noise did not change any pixel")
	}
}

func TestGenerateSequenceZeroObjects(t *testing.T) {
	imgs, truths := GenerateSequence(1, 5, 64, 64, 0, 3)
	if len(imgs) != 5 {
		t.Fatalf("frames = %d", len(imgs))
	}
	for i := range truths {
		if len(truths[i]) != 0 {
			t.Errorf("frame %d has objects with nObjects=0", i)
		}
	}
}

func TestDrawBox(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 20, 20))
	DrawBox(img, Box{X: 2, Y: 2, W: 10, H: 10})
	if r, _, _, _ := img.At(2, 2).RGBA(); r < 0xffff {
		t.Error("box border not drawn")
	}
	if r, _, _, _ := img.At(12, 12).RGBA(); r != 0 {
		t.Error("draw box wrote outside its border")
	}
}
