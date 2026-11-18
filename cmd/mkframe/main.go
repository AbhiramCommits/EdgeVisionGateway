// Command mkframe writes synthetic frames to disk for manual testing and for
// the curl examples in docs/API.md. Frames come from the same deterministic
// generator used by the accuracy harness.
package main

import (
	"flag"
	"fmt"
	"image/jpeg"
	"os"

	"github.com/edgevision/edgevisiongateway/internal/vision"
)

func main() {
	out := flag.String("out", "testdata/frame.jpg", "output JPEG path")
	frame := flag.Int("frame", 20, "frame index in the generated sequence (0-11 are empty warm-up frames)")
	frames := flag.Int("frames", 40, "total frames to generate")
	width := flag.Int("width", 640, "width")
	height := flag.Int("height", 480, "height")
	objects := flag.Int("objects", 4, "objects per frame")
	noise := flag.Float64("noise", 6, "noise sigma")
	seed := flag.Int64("seed", 7, "generator seed")
	quality := flag.Int("quality", 88, "JPEG quality")
	flag.Parse()

	if *frame < 0 || *frame >= *frames {
		fmt.Fprintf(os.Stderr, "-frame must be in [0,%d)\n", *frames)
		os.Exit(1)
	}
	imgs, _ := vision.GenerateSequence(*seed, *frames, *width, *height, *objects, *noise)
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create %s: %v\n", *out, err)
		os.Exit(1)
	}
	defer f.Close()
	if err := jpeg.Encode(f, imgs[*frame], &jpeg.Options{Quality: *quality}); err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		os.Exit(1)
	}
	st, _ := f.Stat()
	fmt.Printf("wrote %s (%d bytes, frame %d)\n", *out, st.Size(), *frame)
}
