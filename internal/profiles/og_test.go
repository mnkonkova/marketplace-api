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
