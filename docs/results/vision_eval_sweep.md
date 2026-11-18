# Detection accuracy: noise sweep

Measured on the build described in `docs/ENVIRONMENT.md` with the command below.

```sh
go run ./cmd/evaldet -frames 300 -objects 4 -noise <N> -seed 7 -out docs/results/vision_eval_noise<N>.json
```

Each row is the same 300-frame synthetic sequence (4 objects, 640x480, seed 7); only the
Gaussian sensor-noise sigma changes. 12 warm-up frames carry no ground truth, so the
maximum possible TP per run is 4 x 288 = 1152.

| Noise sigma | TP | FP | FN | Precision | Recall | F1 | Mean IoU | AP@0.5 | Mean detect (us/frame) |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 0 | 1152 | 0 | 0 | 1.0000 | 1.0000 | 1.0000 | 1.0000 | 1.0000 | 12821 |
| 4 | 1152 | 0 | 0 | 1.0000 | 1.0000 | 1.0000 | 1.0000 | 1.0000 | 13074 |
| 8 | 1152 | 0 | 0 | 1.0000 | 1.0000 | 1.0000 | 1.0000 | 1.0000 | 12545 |
| 16 | 1152 | 0 | 0 | 1.0000 | 1.0000 | 1.0000 | 0.9958 | 1.0000 | 12931 |
| 32 | 1152 | 0 | 0 | 1.0000 | 1.0000 | 1.0000 | 0.8833 | 1.0000 | 15016 |
| 48 | 1068 | 112 | 84 | 0.9051 | 0.9271 | 0.9160 | 0.6469 | 0.9146 | 15135 |
| 64 | 58 | 14090 | 1094 | 0.0041 | 0.0503 | 0.0076 | 0.5742 | 0.0049 | 16597 |

Raw per-run artefacts: `docs/results/vision_eval_noise0.json`, `..._noise4.json`,
`..._noise8.json`, `..._noise16.json`, `..._noise32.json`, `..._noise48.json`,
`..._noise64.json`.

## Interpretation (from the measured rows)

- Up to sigma 32 the detector finds every object: recall is 1.0000 and there are zero false positives,
  because the 3x3 opening removes isolated noise pixels before connected components run.
- Mean IoU starts dropping at sigma 32 (0.8833 vs 1.0000 at sigma 16) as noise enlarges component
  boundaries; AP@0.5 stays at 1.0000 because every box is still matched.
- Sigma 48 is the knee: precision 0.9051 / recall 0.9271 / IoU 0.6469. Noise blobs survive morphology
  and merge with objects, enlarging boxes and creating false components.
- At sigma 64 the frame is dominated by noise (precision 0.0041); the fixed 30/255 threshold and
  250-pixel minimum area are no longer viable. This is the honest operating limit of a fixed-threshold
  background model, not a result to hide.
