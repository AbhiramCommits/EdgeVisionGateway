package vision

import (
	"math"
	"testing"
)

func det(label string, conf float64, x, y, w, h int) Detection {
	return Detection{Label: label, Confidence: conf, Box: Box{X: x, Y: y, W: w, H: h}}
}

func TestIoU(t *testing.T) {
	a := Box{X: 0, Y: 0, W: 10, H: 10}
	cases := []struct {
		name string
		b    Box
		want float64
	}{
		{"identical", a, 1},
		{"disjoint", Box{X: 20, Y: 20, W: 5, H: 5}, 0},
		{"half", Box{X: 0, Y: 0, W: 5, H: 10}, 0.5},
		{"contained", Box{X: 2, Y: 2, W: 4, H: 4}, 0.16},
		{"touching-edge", Box{X: 10, Y: 0, W: 5, H: 10}, 0},
		{"negative-area", Box{X: 0, Y: 0, W: 0, H: 0}, 0},
	}
	for _, c := range cases {
		if got := IoU(a, c.b); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: IoU = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMatchGreedyByConfidence(t *testing.T) {
	truth := []Detection{det("person", 1, 0, 0, 10, 10)}
	pred := []Detection{
		det("person", 0.5, 30, 30, 10, 10), // FP
		det("person", 0.9, 0, 0, 10, 10),   // TP
	}
	tp, fp, fn := Match(pred, truth, 0.5)
	if tp != 1 || fp != 1 || fn != 0 {
		t.Errorf("tp/fp/fn = %d/%d/%d, want 1/1/0", tp, fp, fn)
	}
}

func TestMatchDuplicatePredictionCountsFalsePositive(t *testing.T) {
	truth := []Detection{det("person", 1, 0, 0, 10, 10)}
	pred := []Detection{
		det("person", 0.9, 0, 0, 10, 10),
		det("person", 0.8, 0, 0, 10, 10),
	}
	tp, fp, fn := Match(pred, truth, 0.5)
	if tp != 1 || fp != 1 || fn != 0 {
		t.Errorf("tp/fp/fn = %d/%d/%d, want 1/1/0", tp, fp, fn)
	}
}

func TestMatchMissedTruth(t *testing.T) {
	truth := []Detection{det("person", 1, 0, 0, 10, 10), det("object", 1, 50, 50, 10, 10)}
	pred := []Detection{det("person", 0.9, 0, 0, 10, 10)}
	tp, fp, fn := Match(pred, truth, 0.5)
	if tp != 1 || fp != 0 || fn != 1 {
		t.Errorf("tp/fp/fn = %d/%d/%d, want 1/0/1", tp, fp, fn)
	}
}

func TestMatchWrongIoUThreshold(t *testing.T) {
	truth := []Detection{det("person", 1, 0, 0, 10, 10)}
	pred := []Detection{det("person", 0.9, 0, 0, 5, 10)} // IoU 0.5
	if tp, _, _ := Match(pred, truth, 0.5); tp != 1 {
		t.Errorf("IoU exactly at threshold should match, tp=%d", tp)
	}
	if tp, fp, fn := Match(pred, truth, 0.51); tp != 0 || fp != 1 || fn != 1 {
		t.Errorf("tp/fp/fn = %d/%d/%d, want 0/1/1", tp, fp, fn)
	}
}

func TestMatchWithIOU(t *testing.T) {
	truth := []Detection{det("person", 1, 0, 0, 10, 10)}
	pred := []Detection{det("person", 0.9, 0, 0, 5, 10)}
	tp, _, _, ious := MatchWithIOU(pred, truth, 0.4)
	if tp != 1 || len(ious) != 1 || math.Abs(ious[0]-0.5) > 1e-9 {
		t.Errorf("tp=%d ious=%v, want tp=1 and IoU 0.5", tp, ious)
	}
}

func TestAveragePrecisionPerfect(t *testing.T) {
	all := []Sample{
		{Pred: []Detection{det("person", 0.9, 0, 0, 10, 10)}, Truth: []Detection{det("person", 1, 0, 0, 10, 10)}},
		{Pred: []Detection{det("object", 0.8, 20, 20, 10, 10)}, Truth: []Detection{det("object", 1, 20, 20, 10, 10)}},
	}
	if got := AveragePrecision(all, 0.5); math.Abs(got-1) > 1e-9 {
		t.Errorf("AP = %v, want 1", got)
	}
}

func TestAveragePrecisionAllPoint(t *testing.T) {
	// Predictions sorted by confidence: TP(1.0), FP(0.8), TP(0.7).
	// Precision after each: 1.0, 0.5, 0.667; recall: 0.5, 0.5, 1.0.
	// Right-interpolated precision at recall 0.5 is 1.0, at recall 1.0 is 0.667.
	// AP = 0.5*1.0 + 0.5*0.6667 = 0.8333.
	all := []Sample{{
		Pred: []Detection{
			det("person", 1.0, 0, 0, 10, 10),
			det("person", 0.8, 50, 50, 10, 10),
			det("object", 0.7, 20, 20, 10, 10),
		},
		Truth: []Detection{
			det("person", 1, 0, 0, 10, 10),
			det("object", 1, 20, 20, 10, 10),
		},
	}}
	got := AveragePrecision(all, 0.5)
	want := 0.5*1.0 + 0.5*(2.0/3.0)
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("AP = %.6f, want %.6f", got, want)
	}
}

func TestAveragePrecisionNoTruth(t *testing.T) {
	all := []Sample{{Pred: []Detection{det("person", 0.9, 0, 0, 10, 10)}}}
	if got := AveragePrecision(all, 0.5); got != 0 {
		t.Errorf("AP without truth = %v, want 0", got)
	}
}

func TestAveragePrecisionNoPredictions(t *testing.T) {
	all := []Sample{{Truth: []Detection{det("person", 1, 0, 0, 10, 10)}}}
	if got := AveragePrecision(all, 0.5); got != 0 {
		t.Errorf("AP without predictions = %v, want 0", got)
	}
}
