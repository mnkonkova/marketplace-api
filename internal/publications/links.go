package publications

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Платформы, на которые креатор выкладывает один и тот же ролик.
// Значения совпадают с CHECK в publication_links.platform (00032).
const (
	PlatformTikTok    = "tiktok"
	PlatformInstagram = "instagram"
	PlatformYouTube   = "youtube"
	PlatformVK        = "vk"
	PlatformLikee     = "likee"
)

// AllPlatforms — обязательный набор площадок. Выкладка закрывается, когда
// пришли все пять. Порядок фиксирован: он же порядок колонок в UI.
var AllPlatforms = []string{
	PlatformTikTok, PlatformInstagram, PlatformYouTube, PlatformVK, PlatformLikee,
}

var (
	// ErrUnknownPlatform — ссылка не с одной из пяти площадок. Креатор
	// вставляет что угодно, поэтому ошибка должна быть внятной, а не
	// «invalid input».
	ErrUnknownPlatform = errors.New("ссылка не с одной из пяти площадок")
	ErrNotAURL         = errors.New("это не похоже на ссылку")
)

// hostSuffixes — соответствие домена площадке. Ключ сравнивается как сам
// хост или как его суффикс с точкой (m.vk.com → vk.com), поэтому мобильные
// и региональные поддомены распознаются без отдельных правил.
var hostSuffixes = []struct {
	host     string
	platform string
}{
	{"tiktok.com", PlatformTikTok},
	{"instagram.com", PlatformInstagram},
	{"youtube.com", PlatformYouTube},
	{"youtu.be", PlatformYouTube},
	{"vk.com", PlatformVK},
	{"vk.ru", PlatformVK},
	{"vkvideo.ru", PlatformVK},
	{"likee.video", PlatformLikee},
	{"likee.com", PlatformLikee},
}

// trackingParams — то, что приезжает вместе со ссылкой из мобильного
// приложения и рекламных кабинетов. Эти параметры не влияют на то, какой
// это ролик, но делают две одинаковые ссылки разными строками — а у нас
// UNIQUE (publication_id, platform) и поход в instacurl по url_canonical.
var trackingParams = map[string]bool{
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
	"utm_term": true, "utm_content": true, "utm_id": true,
	"is_from_webapp": true, "sender_device": true, "sender_web_id": true,
	"web_id": true, "_r": true, "_t": true, "checksum": true,
	"share_app_id": true, "share_link_id": true, "share_item_id": true,
	"timestamp": true, "u_code": true, "preview_pb": true, "refer": true,
	"fbclid": true, "gclid": true, "yclid": true, "igshid": true, "img_index": true,
	"si": true, "feature": true, "pp": true, "app": true,
	"from": true, "rand": true, "source": true,
	// sh — метка «поделиться» у VK: vkvideo.ru/clip-...?sh=4. Встретилась
	// на живой ссылке, без неё два адреса одного ролика расходились.
	"sh": true, "z": true, "reply": true, "list": true,
}

// Link — распознанная ссылка.
type Link struct {
	Platform  string
	Raw       string // как вставил креатор
	Canonical string // по этой ходим в instacurl
	MediaID   string // id ролика на площадке, если удалось выделить
}

// ParseLink — распознаёт площадку и приводит ссылку к каноническому виду.
//
// Требование К2: «ссылки принимаем в любом виде — с рекламными хвостами,
// с мобильной версии; распознаём площадку сами». Поэтому здесь терпимость
// к мусору, а не строгая валидация: голый домен без схемы, лишние пробелы
// и хвосты — нормальный ввод.
func ParseLink(raw string) (Link, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Link{}, ErrNotAURL
	}
	// Креатор копирует ссылку из приложения и часто без схемы.
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return Link{}, ErrNotAURL
	}

	host := strings.ToLower(u.Hostname())
	platform := detectPlatform(host)
	if platform == "" {
		return Link{}, fmt.Errorf("%w: %s", ErrUnknownPlatform, host)
	}

	u.Scheme = "https"
	u.Host = normalizeHost(host, platform)
	u.Fragment = ""
	u.User = nil
	stripTracking(u)

	mediaID := extractMediaID(platform, u)
	normalizePath(platform, u, mediaID)

	return Link{
		Platform:  platform,
		Raw:       strings.TrimSpace(raw),
		Canonical: u.String(),
		MediaID:   mediaID,
	}, nil
}

func detectPlatform(host string) string {
	for _, h := range hostSuffixes {
		if host == h.host || strings.HasSuffix(host, "."+h.host) {
			return h.platform
		}
	}
	return ""
}

// normalizeHost — приводит зеркала и мобильные поддомены к одному виду,
// чтобы одна и та же ссылка не превращалась в две разные строки.
func normalizeHost(host, platform string) string {
	switch platform {
	case PlatformVK:
		return "vk.com"
	case PlatformYouTube:
		if host == "youtu.be" || strings.HasSuffix(host, ".youtu.be") {
			return "www.youtube.com"
		}
		return "www.youtube.com"
	case PlatformTikTok:
		// vm./vt. — короткие ссылки, они редиректят; сам редирект
		// разворачивает instacurl, домен не трогаем.
		if strings.HasPrefix(host, "vm.") || strings.HasPrefix(host, "vt.") {
			return host
		}
		return "www.tiktok.com"
	case PlatformInstagram:
		return "www.instagram.com"
	case PlatformLikee:
		return strings.TrimPrefix(host, "l.")
	}
	return host
}

func stripTracking(u *url.URL) {
	q := u.Query()
	for k := range q {
		if trackingParams[strings.ToLower(k)] {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
}

// extractMediaID — id ролика на площадке. Нужен, чтобы одну и ту же
// публикацию, вставленную двумя разными ссылками, можно было опознать.
func extractMediaID(platform string, u *url.URL) string {
	parts := pathParts(u.Path)
	switch platform {
	case PlatformYouTube:
		if v := u.Query().Get("v"); v != "" {
			return v
		}
		// youtu.be/<id>, /shorts/<id>, /live/<id>
		for i, p := range parts {
			if (p == "shorts" || p == "live" || p == "embed") && i+1 < len(parts) {
				return parts[i+1]
			}
		}
		if len(parts) == 1 {
			return parts[0]
		}
	case PlatformTikTok:
		for i, p := range parts {
			if p == "video" && i+1 < len(parts) {
				return parts[i+1]
			}
		}
	case PlatformInstagram:
		for i, p := range parts {
			if (p == "p" || p == "reel" || p == "reels" || p == "tv") && i+1 < len(parts) {
				return parts[i+1]
			}
		}
	case PlatformVK:
		// vk.com/video-123_456, /clip-123_456, /wall-123_456
		for _, p := range parts {
			for _, pref := range []string{"video", "clip", "wall"} {
				if strings.HasPrefix(p, pref) && len(p) > len(pref) {
					return strings.TrimPrefix(p, pref)
				}
			}
		}
	case PlatformLikee:
		// likee.video/@user/video/123456 и /v/<code>
		for i, p := range parts {
			if (p == "video" || p == "v") && i+1 < len(parts) {
				return parts[i+1]
			}
		}
	}
	return ""
}

// normalizePath — приводит разные формы одной ссылки к одной. Сейчас это
// нужно только для YouTube: youtu.be/<id> и watch?v=<id> — один ролик.
func normalizePath(platform string, u *url.URL, mediaID string) {
	if platform != PlatformYouTube || mediaID == "" {
		if u.Path == "" {
			u.Path = "/"
		}
		u.Path = strings.TrimSuffix(u.Path, "/")
		if u.Path == "" {
			u.Path = "/"
		}
		return
	}
	parts := pathParts(u.Path)
	// Shorts оставляем как есть: это отдельный формат, и подменять его на
	// /watch — врать о том, что выложил креатор.
	for _, p := range parts {
		if p == "shorts" {
			u.Path = "/shorts/" + mediaID
			u.RawQuery = ""
			return
		}
	}
	u.Path = "/watch"
	q := u.Query()
	q.Set("v", mediaID)
	u.RawQuery = q.Encode()
}

func pathParts(p string) []string {
	out := make([]string, 0, 4)
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// IsKnownPlatform — площадка из обязательной пятёрки.
func IsKnownPlatform(p string) bool {
	for _, known := range AllPlatforms {
		if known == p {
			return true
		}
	}
	return false
}
