package vision

import (
	"testing"
)

// BenchmarkDetect measures the full pure-Go pipeline on 640x480 and 1280x720
// frames. Two frames with moving objects are alternated so the detector cannot
// short-circuit on a static scene.
func BenchmarkDetect(b *testing.B) {
	sizes := []struct {
		name   string
		width  int
		height int
	}{
		{"640x480", 640, 480},
		{"1280x720", 1280, 720},
	}
	for _, size := range sizes {
		b.Run(size.name, func(b *testing.B) {
			imgs, _ := GenerateSequence(7, 22, size.width, size.height, 4, 5)
			frames := imgs[20:22]
			d := NewMotionDetector()
			if _, err := d.Detect(imgs[0]); err != nil { // bootstrap background
				b.Fatalf("bootstrap: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := d.Detect(frames[i%len(frames)]); err != nil {
					b.Fatalf("detect: %v", err)
				}
			}
		})
	}
}
