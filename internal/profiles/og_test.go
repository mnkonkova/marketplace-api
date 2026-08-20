package profiles

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func testProfile() PublicProfile {
	return PublicProfile{
		UserID:      uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Username:    "wayprod",
		DisplayName: "WAYPROD.",
		City:        "Москва",
		Categories: []CategoryRef{
			{Code: "director", Title: "Режиссёр", IsPrimary: true},
			{Code: "editor", Title: "Монтажёр"},
			{Code: "motion", Title: "Моушн-дизайнер"},
			{Code: "color", Title: "Колорист"},
		},
		Portfolio: []PortfolioItem{
			{Title: "Обычная", ThumbnailURL: "https://cdn/first.jpg"},
			{Title: "Промо", ThumbnailURL: "https://cdn/featured.jpg", IsFeatured: true},
		},
		ProductionName: "WAYPROD",
	}
}

func TestBuildOGMeta_TitleHasNameAndTopRoles(t *testing.T) {
	m := buildOGMeta(testProfile(), "https://wayprmarket.ru")
	// Три роли, не все четыре: в карточке мессенджера длинный заголовок
	// обрезается, и лишние роли вытесняют имя.
	want := "WAYPROD. — режиссёр, монтажёр, моушн-дизайнер"
	if m.Title != want {
		t.Errorf("og:title = %q, ожидали %q", m.Title, want)
	}
}

func TestBuildOGMeta_Description(t *testing.T) {
	m := buildOGMeta(testProfile(), "https://wayprmarket.ru")
	want := "Москва · WAYPROD · 2 работы в портфолио"
	if m.Description != want {
		t.Errorf("og:description = %q, ожидали %q", m.Description, want)
	}
}

// Картинка превью — постер закреплённой работы: именно её специалист
// выбрал визиткой, а не первую попавшуюся.
func TestBuildOGMeta_ImagePrefersFeatured(t *testing.T) {
	if got := buildOGMeta(testProfile(), "https://x").Image; got != "https://cdn/featured.jpg" {
		t.Errorf("og:image = %q, ожидали постер закреплённой работы", got)
	}
}

func TestBuildOGMeta_ImageFallsBackToAvatar(t *testing.T) {
	p := testProfile()
	p.Portfolio = nil
	p.AvatarURL = "https://cdn/avatar.jpg"
	if got := buildOGMeta(p, "https://x").Image; got != "https://cdn/avatar.jpg" {
		t.Errorf("без работ ожидали аватар, получили %q", got)
	}
}

// og:url обязан быть абсолютным — относительный это самая частая причина
// «превью без картинки и без ссылки».
func TestBuildOGMeta_CanonicalURL(t *testing.T) {
	m := buildOGMeta(testProfile(), "https://wayprmarket.ru/")
	if m.URL != "https://wayprmarket.ru/specialist/wayprod" {
		t.Errorf("og:url = %q", m.URL)
	}
	p := testProfile()
	p.Username = ""
	m = buildOGMeta(p, "https://wayprmarket.ru")
	if !strings.HasSuffix(m.URL, "/specialist/11111111-1111-1111-1111-111111111111") {
		t.Errorf("без username ожидали id в URL, получили %q", m.URL)
	}
}

func TestBuildOGMeta_FreelancerWithoutCity(t *testing.T) {
	p := testProfile()
	p.City = ""
	p.ProductionName = ""
	p.IsFreelance = true
	p.Portfolio = p.Portfolio[:1]
	m := buildOGMeta(p, "https://x")
	if m.Description != "Удалённо · фриланс · 1 работа в портфолио" {
		t.Errorf("описание = %q", m.Description)
	}
}

const shell = `<!doctype html><html><head>
    <title>Wayprmarket</title>
    <meta name="description" content="общее описание сайта" />
    <meta property="og:title" content="Wayprmarket — подбор специалистов" />
    <meta property="og:url" content="https://wayprmarket.ru" />
  </head><body><app-root></app-root></body></html>`

func TestInjectOG_ReplacesSiteWideTags(t *testing.T) {
	out := injectOG(shell, buildOGMeta(testProfile(), "https://wayprmarket.ru"))

	// Общие теги сайта должны исчезнуть: два og:title в документе — и какой
	// из них возьмёт превьюшник, не определено.
	if strings.Contains(out, "Wayprmarket — подбор специалистов") {
		t.Error("общий og:title сайта остался в документе")
	}
	if strings.Count(out, `property="og:title"`) != 1 {
		t.Errorf("ожидали ровно один og:title, нашли %d", strings.Count(out, `property="og:title"`))
	}
	if !strings.Contains(out, `content="WAYPROD. — режиссёр, монтажёр, моушн-дизайнер"`) {
		t.Error("нет og:title специалиста")
	}
	if !strings.Contains(out, `content="https://cdn/featured.jpg"`) {
		t.Error("нет og:image")
	}
	if !strings.Contains(out, `name="twitter:card" content="summary_large_image"`) {
		t.Error("нет twitter:card")
	}
}

func TestInjectOG_KeepsAppMarkup(t *testing.T) {
	out := injectOG(shell, buildOGMeta(testProfile(), "https://x"))
	if !strings.Contains(out, "<app-root></app-root>") {
		t.Error("оболочка SPA повреждена — приложение не загрузится")
	}
	if strings.Index(out, "og:title") > strings.Index(out, "</head>") {
		t.Error("мета вставлена вне <head>")
	}
}

// Кавычки и угловые скобки в имени не должны разрывать атрибут и
// превращаться в разметку.
func TestInjectOG_EscapesUserContent(t *testing.T) {
	p := testProfile()
	p.DisplayName = `Студия "Кавычка" <script>alert(1)</script>`
	out := injectOG(shell, buildOGMeta(p, "https://x"))
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Error("пользовательский текст попал в HTML без экранирования")
	}
}

func TestInjectOG_NoHeadReturnsShellUnchanged(t *testing.T) {
	broken := "<html><body>нет head</body></html>"
	if got := injectOG(broken, ogMeta{Title: "x"}); got != broken {
		t.Error("без </head> оболочку нужно вернуть нетронутой, а не ломать")
	}
}

// <title> тоже подменяем: og:title читают мессенджеры, а <title> уходит в
// поисковую выдачу и в заголовок вкладки. SPA проставит его сама, но
// только после загрузки JS — которого у бота нет.
func TestInjectOG_ReplacesTitle(t *testing.T) {
	out := injectOG(shell, buildOGMeta(testProfile(), "https://x"))
	if strings.Contains(out, "<title>Wayprmarket</title>") {
		t.Error("общий <title> сайта остался")
	}
	if !strings.Contains(out, "<title>WAYPROD. — режиссёр, монтажёр, моушн-дизайнер · wayprmarket</title>") {
		t.Errorf("не нашли персональный <title>")
	}
}

// Размеры og:image раньше были захардкожены 1280x720. Для вертикального
// постера (9:16 — типичный промо-ролик) мета сообщала краулеру горизонтальную
// пропорцию, и Telegram/VK показывали ссылку без превью.
func TestOGImageSize(t *testing.T) {
	cases := []struct {
		aspect string
		w, h   int
	}{
		{"9:16", 720, 1280},
		{"16:9", 1280, 720},
		{"1:1", 1280, 1280},
		{"4:5", 1024, 1280},
		{"", 0, 0},
		{"abc", 0, 0},
		{"16", 0, 0},
		{"16:0", 0, 0},
		{"0:9", 0, 0},
		{"-16:9", 0, 0},
	}
	for _, c := range cases {
		w, h := ogImageSize(c.aspect)
		if w != c.w || h != c.h {
			t.Errorf("ogImageSize(%q) = %d x %d, ожидалось %d x %d", c.aspect, w, h, c.w, c.h)
		}
	}
}

// Неизвестный аспект (работа старше миграции 00028, либо аватар) — теги
// размеров не пишем вовсе: краулер измерит картинку сам.
func TestInjectOGSkipsUnknownImageSize(t *testing.T) {
	shell := "<html><head><title>x</title></head><body></body></html>"

	out := injectOG(shell, ogMeta{Title: "T", Image: "https://cdn/x.jpg"})
	if strings.Contains(out, "og:image:width") {
		t.Error("размеры объявлены, хотя аспект неизвестен")
	}
	if !strings.Contains(out, `content="https://cdn/x.jpg"`) {
		t.Error("сама картинка потерялась")
	}

	out = injectOG(shell, ogMeta{Title: "T", Image: "https://cdn/x.jpg", ImageW: 720, ImageH: 1280})
	if !strings.Contains(out, `content="720"`) || !strings.Contains(out, `content="1280"`) {
		t.Error("известные размеры не попали в мету")
	}
}

// Вертикальная закреплённая работа должна давать вертикальные размеры.
func TestBuildOGMetaVerticalFeatured(t *testing.T) {
	p := PublicProfile{
		DisplayName: "Аня",
		Portfolio: []PortfolioItem{
			{ThumbnailURL: "https://cdn/wide.jpg", Aspect: "16:9"},
			{ThumbnailURL: "https://cdn/tall.jpg", Aspect: "9:16", IsFeatured: true},
		},
	}
	m := buildOGMeta(p, "https://wayprmarket.ru")
	if m.Image != "https://cdn/tall.jpg" {
		t.Fatalf("выбрана не закреплённая работа: %s", m.Image)
	}
	if m.ImageW != 720 || m.ImageH != 1280 {
		t.Errorf("размеры %d x %d, ожидалось 720 x 1280", m.ImageW, m.ImageH)
	}
}
