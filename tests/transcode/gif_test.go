package transcode_test

import (
	"testing"

	"marketpclce/internal/transcode"
)

// ChooseGifParams — pure-function unit-тест по таблице бакетов
// (см. docs/VIDEO_TRANSCODING.md §11). Цель — 50-120 кадров суммарно
// чтобы animated WebP уложился в 50-150 KB.
func TestChooseGifParams(t *testing.T) {
	tests := []struct {
		name     string
		duration float64
		want     transcode.GifParams
	}{
		{
			name:     "very short (≤5 sec) — full file, fps 12, q 75",
			duration: 3.5,
			want:     transcode.GifParams{OffsetSec: 0, DurationSec: 0, FPS: 12, Quality: 75},
		},
		{
			name:     "exactly 5 sec — short bucket (boundary inclusive)",
			duration: 5.0,
			want:     transcode.GifParams{OffsetSec: 0, DurationSec: 0, FPS: 12, Quality: 75},
		},
		{
			name:     "medium (5-15 sec) — skip 2s, fps 12, q 70",
			duration: 10,
			want:     transcode.GifParams{OffsetSec: 2, DurationSec: 0, FPS: 12, Quality: 70},
		},
		{
			name:     "exactly 15 sec — middle bucket",
			duration: 15,
			want:     transcode.GifParams{OffsetSec: 2, DurationSec: 0, FPS: 12, Quality: 70},
		},
		{
			name:     "long (>15 sec) — 10s clip with offset 2, fps 10, q 65",
			duration: 30,
			want:     transcode.GifParams{OffsetSec: 2, DurationSec: 10, FPS: 10, Quality: 65},
		},
		{
			name:     "very long (5 min) — long bucket",
			duration: 300,
			want:     transcode.GifParams{OffsetSec: 2, DurationSec: 10, FPS: 10, Quality: 65},
		},
		{
			name:     "edge: 0 duration (probe failed in caller) — short bucket",
			duration: 0,
			want:     transcode.GifParams{OffsetSec: 0, DurationSec: 0, FPS: 12, Quality: 75},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := transcode.ChooseGifParams(tc.duration)
			if got != tc.want {
				t.Errorf("ChooseGifParams(%v) = %+v, want %+v", tc.duration, got, tc.want)
			}
		})
	}
}
