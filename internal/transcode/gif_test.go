package transcode

import (
	"testing"
)

func TestChooseGifParams(t *testing.T) {
	tests := []struct {
		name     string
		duration float64
		want     GifParams
	}{
		{
			name:     "very short (under 5 sec) — full file, fps 12, q 75",
			duration: 3.5,
			want:     GifParams{OffsetSec: 0, DurationSec: 0, FPS: 12, Quality: 75},
		},
		{
			name:     "exactly 5 sec — full file (boundary inclusive)",
			duration: 5.0,
			want:     GifParams{OffsetSec: 0, DurationSec: 0, FPS: 12, Quality: 75},
		},
		{
			name:     "medium (5-15 sec) — skip 2s, full minus skip, fps 12, q 70",
			duration: 10,
			want:     GifParams{OffsetSec: 2, DurationSec: 0, FPS: 12, Quality: 70},
		},
		{
			name:     "exactly 15 sec — middle bucket",
			duration: 15,
			want:     GifParams{OffsetSec: 2, DurationSec: 0, FPS: 12, Quality: 70},
		},
		{
			name:     "long (>15 sec) — 10s clip with offset 2, fps 10, q 65",
			duration: 30,
			want:     GifParams{OffsetSec: 2, DurationSec: 10, FPS: 10, Quality: 65},
		},
		{
			name:     "very long (5 min) — same long bucket",
			duration: 300,
			want:     GifParams{OffsetSec: 2, DurationSec: 10, FPS: 10, Quality: 65},
		},
		{
			name:     "edge case: 0 duration (probe failed elsewhere) — short bucket",
			duration: 0,
			want:     GifParams{OffsetSec: 0, DurationSec: 0, FPS: 12, Quality: 75},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ChooseGifParams(tc.duration)
			if got != tc.want {
				t.Errorf("ChooseGifParams(%v) = %+v, want %+v", tc.duration, got, tc.want)
			}
		})
	}
}

func TestAnimatedThumbKeyFor(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"standard mp4", "portfolio/abc/123.mp4", "portfolio/abc/123_preview.webp"},
		{"mov input", "portfolio/abc/123.mov", "portfolio/abc/123_preview.webp"},
		{"no extension", "portfolio/abc/123", "portfolio/abc/123_preview.webp"},
		{"uppercase ext", "portfolio/abc/123.MP4", "portfolio/abc/123_preview.webp"},
		{"nested path", "portfolio/u/sub/x/item.mp4", "portfolio/u/sub/x/item_preview.webp"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := animatedThumbKeyFor(tc.in); got != tc.want {
				t.Errorf("animatedThumbKeyFor(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
