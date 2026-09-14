package publications

import (
	"errors"
	"testing"
)

// Кейсы взяты из того, что реально вставляет креатор: ссылка из мобильного
// приложения с рекламным хвостом, короткая ссылка «поделиться», зеркало
// домена. Требование К2 — принимать это всё и распознавать площадку самим.
func TestParseLink(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		platform  string
		canonical string
		mediaID   string
	}{
		{
			name:      "tiktok из приложения с хвостом",
			in:        "https://www.tiktok.com/@creator/video/7412345678901234567?is_from_webapp=1&sender_device=pc&_r=1",
			platform:  PlatformTikTok,
			canonical: "https://www.tiktok.com/@creator/video/7412345678901234567",
			mediaID:   "7412345678901234567",
		},
		{
			name:      "tiktok короткая ссылка остаётся короткой",
			in:        "https://vm.tiktok.com/ZMabcdef/",
			platform:  PlatformTikTok,
			canonical: "https://vm.tiktok.com/ZMabcdef",
		},
		{
			name:      "youtu.be сводится к watch",
			in:        "https://youtu.be/dQw4w9WgXcQ?si=abcdef&t=15",
			platform:  PlatformYouTube,
			canonical: "https://www.youtube.com/watch?t=15&v=dQw4w9WgXcQ",
			mediaID:   "dQw4w9WgXcQ",
		},
		{
			name:      "youtube watch с мусором",
			in:        "https://m.youtube.com/watch?v=dQw4w9WgXcQ&feature=share&utm_source=tg",
			platform:  PlatformYouTube,
			canonical: "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			mediaID:   "dQw4w9WgXcQ",
		},
		{
			name:      "shorts остаётся shorts",
			in:        "https://www.youtube.com/shorts/abc123XYZ?feature=share",
			platform:  PlatformYouTube,
			canonical: "https://www.youtube.com/shorts/abc123XYZ",
			mediaID:   "abc123XYZ",
		},
		{
			name:      "instagram reel",
			in:        "https://www.instagram.com/reel/C8xYzAbCdEf/?igshid=xxx&img_index=1",
			platform:  PlatformInstagram,
			canonical: "https://www.instagram.com/reel/C8xYzAbCdEf",
			mediaID:   "C8xYzAbCdEf",
		},
		{
			// Реальная ссылка «поделиться» из приложения VK.
			name:      "vkvideo.ru с меткой sh",
			in:        "https://vkvideo.ru/clip-211423460_456239747?sh=4",
			platform:  PlatformVK,
			canonical: "https://vk.com/clip-211423460_456239747",
			mediaID:   "-211423460_456239747",
		},
		{
			name:      "vk.ru это vk.com",
			in:        "https://vk.ru/video-12345_678901",
			platform:  PlatformVK,
			canonical: "https://vk.com/video-12345_678901",
			mediaID:   "-12345_678901",
		},
		{
			name:      "мобильный vk с utm",
			in:        "https://m.vk.com/clip-99_11?utm_campaign=x&from=feed",
			platform:  PlatformVK,
			canonical: "https://vk.com/clip-99_11",
			mediaID:   "-99_11",
		},
		{
			name:      "likee с коротким доменом",
			in:        "https://l.likee.video/v/AbCdEf",
			platform:  PlatformLikee,
			canonical: "https://likee.video/v/AbCdEf",
			mediaID:   "AbCdEf",
		},
		{
			name:      "без схемы и с пробелами",
			in:        "  tiktok.com/@user/video/123  ",
			platform:  PlatformTikTok,
			canonical: "https://www.tiktok.com/@user/video/123",
			mediaID:   "123",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseLink(c.in)
			if err != nil {
				t.Fatalf("ParseLink(%q) вернул ошибку: %v", c.in, err)
			}
			if got.Platform != c.platform {
				t.Errorf("площадка: got %q, want %q", got.Platform, c.platform)
			}
			if got.Canonical != c.canonical {
				t.Errorf("канон: got %q, want %q", got.Canonical, c.canonical)
			}
			if c.mediaID != "" && got.MediaID != c.mediaID {
				t.Errorf("media id: got %q, want %q", got.MediaID, c.mediaID)
			}
			if got.Raw == "" {
				t.Error("Raw должен сохранять то, что вставил креатор")
			}
		})
	}
}

// Одна и та же публикация, вставленная разными ссылками, должна давать
// одинаковый канон — иначе UNIQUE (publication_id, platform) не спасёт от
// дубля, а instacurl соберёт один ролик дважды и сожжёт кредиты.
func TestParseLinkCanonicalIsStable(t *testing.T) {
	variants := []string{
		"https://youtu.be/dQw4w9WgXcQ",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://m.youtube.com/watch?v=dQw4w9WgXcQ&feature=share",
		"youtube.com/watch?v=dQw4w9WgXcQ&utm_source=tg",
	}
	var first string
	for i, v := range variants {
		got, err := ParseLink(v)
		if err != nil {
			t.Fatalf("ParseLink(%q): %v", v, err)
		}
		if i == 0 {
			first = got.Canonical
			continue
		}
		if got.Canonical != first {
			t.Errorf("%q дал канон %q, а первый вариант — %q", v, got.Canonical, first)
		}
	}
}

func TestParseLinkRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"пусто", "   ", ErrNotAURL},
		{"чужая площадка", "https://rutube.ru/video/abc/", ErrUnknownPlatform},
		{"телеграм не входит в пятёрку", "https://t.me/channel/123", ErrUnknownPlatform},
		// Текст с пробелом — это вообще не ссылка; одно слово парсится как
		// хост, и тогда претензия уже к площадке. Разные ошибки — разные
		// подсказки креатору.
		{"просто текст", "завтра выложу", ErrNotAURL},
		{"одно слово", "перезалью", ErrUnknownPlatform},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseLink(c.in)
			if !errors.Is(err, c.want) {
				t.Errorf("ParseLink(%q): got %v, want %v", c.in, err, c.want)
			}
		})
	}
}

func TestAllPlatformsMatchesCheckConstraint(t *testing.T) {
	// Если пятёрка изменится, CHECK в 00032 и этот список должны меняться
	// вместе. Тест ловит расхождение до того, как оно доедет до БД.
	if len(AllPlatforms) != 5 {
		t.Fatalf("ожидалось 5 обязательных площадок, а их %d", len(AllPlatforms))
	}
	for _, p := range AllPlatforms {
		if !IsKnownPlatform(p) {
			t.Errorf("%q есть в AllPlatforms, но IsKnownPlatform его не знает", p)
		}
	}
}
