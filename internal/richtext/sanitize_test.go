package richtext

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func allowAll(uuid.UUID) bool { return true }

// ---- ТЕСТ: разрешённое форматирование доезжает целиком ----

func TestSanitizeKeepsAllowedFormatting(t *testing.T) {
	in := `<p>Смотри <strong>сюда</strong> и <em>сюда</em></p><ul><li>раз</li><li>два</li></ul>`
	got, err := Sanitize(in, allowAll)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if got.HTML != in {
		t.Errorf("html изменился:\n want %s\n got  %s", in, got.HTML)
	}
	if want := "Смотри сюда и сюда\n• раз\n• два"; got.Text != want {
		t.Errorf("text:\n want %q\n got  %q", want, got.Text)
	}
}

// b и i пишут разные редакторы; храним одно написание, иначе сравнивать
// сохранённое с ожидаемым придётся с оговорками в каждом тесте.
func TestSanitizeNormalizesBoldItalic(t *testing.T) {
	got, err := Sanitize(`<b>жирный</b> <i>косой</i>`, allowAll)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if want := `<strong>жирный</strong> <em>косой</em>`; got.HTML != want {
		t.Errorf("want %s, got %s", want, got.HTML)
	}
}

// ---- ТЕСТ: чего не должно остаться ----

func TestSanitizeDropsDangerous(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// mustNot — подстроки, которых в результате быть не может.
		mustNot []string
		// mustHave — текст, который обязан уцелеть: чистка не должна
		// съедать написанное человеком.
		mustHave string
	}{
		{
			name:     "script",
			in:       `привет<script>alert(1)</script>`,
			mustNot:  []string{"script", "alert"},
			mustHave: "привет",
		},
		{
			name:    "обработчик события",
			in:      `<p onclick="steal()">текст</p>`,
			mustNot: []string{"onclick", "steal"},
			// Сам <p> разрешён — выкидывается только атрибут.
			mustHave: "текст",
		},
		{
			name:    "img с onerror",
			in:      `<img src=x onerror=alert(1)>подпись`,
			mustNot: []string{"img", "onerror", "alert"},
			// img не в белом списке и разворачивается: содержимого у него
			// нет, поэтому остаётся только соседний текст.
			mustHave: "подпись",
		},
		{
			name:     "javascript: в ссылке",
			in:       `<a href="javascript:alert(1)">клик</a>`,
			mustNot:  []string{"javascript", "href"},
			mustHave: "клик",
		},
		{
			name:     "javascript: с пробелами и регистром",
			in:       `<a href="  JaVaScRiPt:alert(1)">клик</a>`,
			mustNot:  []string{"avaScr", "href"},
			mustHave: "клик",
		},
		{
			name:     "data: url",
			in:       `<a href="data:text/html;base64,PHNjcmlwdD4=">клик</a>`,
			mustNot:  []string{"data:", "href"},
			mustHave: "клик",
		},
		{
			name:     "iframe",
			in:       `<iframe src="https://evil.example"></iframe>текст`,
			mustNot:  []string{"iframe", "evil"},
			mustHave: "текст",
		},
		{
			name:     "style",
			in:       `<style>body{display:none}</style>текст`,
			mustNot:  []string{"style", "display"},
			mustHave: "текст",
		},
		{
			name:     "экранирование угловых скобок в тексте",
			in:       `1 &lt; 2 &amp; 3 &gt; 2`,
			mustNot:  []string{"<script"},
			mustHave: "1 < 2 & 3 > 2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Sanitize(tc.in, allowAll)
			if err != nil {
				t.Fatalf("sanitize: %v", err)
			}
			low := strings.ToLower(got.HTML)
			for _, bad := range tc.mustNot {
				if strings.Contains(low, strings.ToLower(bad)) {
					t.Errorf("в результате осталось %q: %s", bad, got.HTML)
				}
			}
			if !strings.Contains(got.Text, tc.mustHave) {
				t.Errorf("текст %q потерялся: %q", tc.mustHave, got.Text)
			}
		})
	}
}

// Ссылка остаётся ссылкой, но с rel — без noopener открытая вкладка
// получает доступ к window.opener страницы CRM.
func TestSanitizeKeepsSafeLinkWithRel(t *testing.T) {
	got, err := Sanitize(`<a href="https://vk.com/clip123">ролик</a>`, allowAll)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	for _, want := range []string{`href="https://vk.com/clip123"`, `rel="nofollow noopener noreferrer"`, `target="_blank"`} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("нет %s в %s", want, got.HTML)
		}
	}
}

// ---- ТЕСТ: упоминания ----

func TestSanitizeCollectsAllowedMentions(t *testing.T) {
	u1, u2 := uuid.New(), uuid.New()
	in := `<span ` + MentionAttr + `="` + u1.String() + `">@Аня</span> и ` +
		`<span ` + MentionAttr + `="` + u2.String() + `">@Боря</span>, ` +
		`а ещё <span ` + MentionAttr + `="` + u1.String() + `">@Аня</span>`
	got, err := Sanitize(in, allowAll)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if len(got.Mentions) != 2 {
		t.Fatalf("want 2 упоминания без повторов, got %d: %v", len(got.Mentions), got.Mentions)
	}
	if got.Mentions[0] != u1 || got.Mentions[1] != u2 {
		t.Errorf("порядок появления не сохранён: %v", got.Mentions)
	}
}

// Упомянуть можно только того, кто эту ветку и так видит. Постороннего не
// упоминаем, но и текст сообщения не портим — разворачиваем span.
func TestSanitizeUnwrapsForbiddenMention(t *testing.T) {
	outsider := uuid.New()
	got, err := Sanitize(`привет <span `+MentionAttr+`="`+outsider.String()+`">@Чужой</span>`,
		func(uuid.UUID) bool { return false })
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if len(got.Mentions) != 0 {
		t.Errorf("упоминание постороннего записалось: %v", got.Mentions)
	}
	if strings.Contains(got.HTML, MentionAttr) {
		t.Errorf("span остался: %s", got.HTML)
	}
	if !strings.Contains(got.Text, "@Чужой") {
		t.Errorf("текст упоминания пропал: %q", got.Text)
	}
}

func TestSanitizeIgnoresBrokenMentionID(t *testing.T) {
	got, err := Sanitize(`<span `+MentionAttr+`="не-uuid">@кто-то</span>`, allowAll)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if len(got.Mentions) != 0 {
		t.Errorf("кривой id попал в упоминания: %v", got.Mentions)
	}
	if !strings.Contains(got.Text, "@кто-то") {
		t.Errorf("текст пропал: %q", got.Text)
	}
}

// Ветка, где упоминания не разрешены вовсе (mentionAllowed == nil).
func TestSanitizeNilMentionPredicate(t *testing.T) {
	got, err := Sanitize(`<span `+MentionAttr+`="`+uuid.New().String()+`">@Аня</span>`, nil)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if len(got.Mentions) != 0 {
		t.Errorf("упоминания собрались без предиката: %v", got.Mentions)
	}
}

// ---- ТЕСТ: границы ----

func TestSanitizeRejectsEmptyAfterCleanup(t *testing.T) {
	for _, in := range []string{``, `   `, `<p></p>`, `<script>alert(1)</script>`} {
		if _, err := Sanitize(in, allowAll); !errors.Is(err, ErrEmpty) {
			t.Errorf("%q: want ErrEmpty, got %v", in, err)
		}
	}
}

func TestSanitizeRejectsHugeInput(t *testing.T) {
	if _, err := Sanitize(strings.Repeat("a", maxInput+1), allowAll); !errors.Is(err, ErrTooLarge) {
		t.Errorf("want ErrTooLarge, got %v", err)
	}
}

// Глубокая вложенность — не форматирование, а нагрузка на браузер читателя.
// Текст при этом обязан уцелеть.
func TestSanitizeFlattensDeepNesting(t *testing.T) {
	deep := strings.Repeat("<strong>", 200) + "текст" + strings.Repeat("</strong>", 200)
	got, err := Sanitize(deep, allowAll)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if n := strings.Count(got.HTML, "<strong>"); n > maxDepth {
		t.Errorf("вложенность не ограничена: %d уровней", n)
	}
	if got.Text != "текст" {
		t.Errorf("текст потерялся: %q", got.Text)
	}
}

// Повторная чистка уже очищенного ничего не меняет: сохранённое тело
// проходит через Sanitize снова при каждом редактировании.
func TestSanitizeIsIdempotent(t *testing.T) {
	in := `<p>раз <strong>два</strong> <a href="https://example.com/x">три</a></p>`
	once, err := Sanitize(in, allowAll)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	twice, err := Sanitize(once.HTML, allowAll)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if once.HTML != twice.HTML {
		t.Errorf("не идемпотентно:\n1: %s\n2: %s", once.HTML, twice.HTML)
	}
}

func TestPlainTextCollapsesWhitespace(t *testing.T) {
	if got := PlainText("  раз   два\n\n\nтри  "); got != "раз два\nтри" {
		t.Errorf("got %q", got)
	}
}
