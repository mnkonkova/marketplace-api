package transcode_test

import (
	"testing"

	"marketpclce/internal/transcode"
)

// ChooseGifParams — pure-function unit-тест по таблице бакетов
// (см. docs/VIDEO_TRANSCODING.md §11). После tune-апа 2026-06: цель ~180-280 KB
// при scale 320, fps 12-15, quality 78-85.
func TestChooseGifParams(t *testing.T) {
	tests := []struct {
		name     string
		duration float64
		want     transcode.GifParams
	}{
		{
			name:     "very short (≤5 sec) — full file, fps 15, q 85",
			duration: 3.5,
			want:     transcode.GifParams{OffsetSec: 0, DurationSec: 0, FPS: 15, Quality: 85},
		},
		{
			name:     "exactly 5 sec — short bucket (boundary inclusive)",
			duration: 5.0,
			want:     transcode.GifParams{OffsetSec: 0, DurationSec: 0, FPS: 15, Quality: 85},
		},
		{
			name:     "medium (5-15 sec) — skip 2s, fps 15, q 82",
			duration: 10,
			want:     transcode.GifParams{OffsetSec: 2, DurationSec: 0, FPS: 15, Quality: 82},
		},
		{
			name:     "exactly 15 sec — middle bucket",
			duration: 15,
			want:     transcode.GifParams{OffsetSec: 2, DurationSec: 0, FPS: 15, Quality: 82},
		},
		{
			name:     "long (>15 sec) — 10s clip with offset 2, fps 12, q 78",
			duration: 30,
			want:     transcode.GifParams{OffsetSec: 2, DurationSec: 10, FPS: 12, Quality: 78},
		},
		{
			name:     "very long (5 min) — long bucket",
			duration: 300,
			want:     transcode.GifParams{OffsetSec: 2, DurationSec: 10, FPS: 12, Quality: 78},
		},
		{
			name:     "edge: 0 duration (probe failed in caller) — short bucket",
			duration: 0,
			want:     transcode.GifParams{OffsetSec: 0, DurationSec: 0, FPS: 15, Quality: 85},
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
