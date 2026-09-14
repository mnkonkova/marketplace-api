// Package richtext чистит размеченные сообщения переписки в проекте.
//
// Переписка допускает жирный, курсив, списки, ссылки и упоминания — то есть
// в теле комментария приезжает HTML, написанный редактором в браузере. Верить
// ему нельзя: тело уходит в базу, а оттуда возвращается всем участникам
// проекта, и один <script> или <img onerror> в комментарии — это XSS у
// менеджера в CRM.
//
// Чистим разбором, а не регулярками. Регулярное выражение не знает про
// незакрытые теги, про <SCRIPT/src=...> и про пробелы внутри имени атрибута,
// и обходится любым из полудюжины известных приёмов. Здесь документ
// разбирается парсером (тем же, что в браузере), дерево обходится, и на
// выход собирается НОВАЯ разметка только из того, что разрешено. Всё
// неизвестное не «вычищается» из строки, а просто не попадает в результат —
// разница принципиальная.
package richtext

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// MentionAttr — атрибут, которым редактор помечает упоминание.
const MentionAttr = "data-mention-user-id"

var (
	// ErrTooLarge — на входе больше, чем мы готовы разбирать. Проверяется
	// ДО парсинга: разбор мегабайтной строки стоит памяти, а осмысленного
	// сообщения такого размера не бывает.
	ErrTooLarge = errors.New("richtext: input too large")
	// ErrEmpty — после чистки не осталось ничего, кроме разметки. Сообщение
	// из одних тегов — это пустое сообщение, а не сообщение.
	ErrEmpty = errors.New("richtext: nothing left after sanitize")
)

const (
	// maxInput — предел на сырой вход. Тело ограничено 5000 символами
	// текста, но разметка вокруг него тоже занимает место, поэтому запас
	// кратный, а не впритык.
	maxInput = 64 * 1024
	// maxDepth — глубже не ходим. Тысяча вложенных <b> — не форматирование,
	// а попытка нагрузить и парсер, и браузер читателя. Всё, что глубже,
	// разворачивается в текст.
	maxDepth = 20
)

// Result — итог чистки.
type Result struct {
	// HTML — то, что храним и отдаём. Уже безопасно вставлять как разметку.
	HTML string
	// Text — тот же текст без единого тега. Для ленты активности, payload
	// в n8n и превью в боте, где HTML показывать нечем.
	Text string
	// Mentions — упомянутые пользователи в порядке появления, без повторов.
	Mentions []uuid.UUID
}

// inline — теги, которые оставляем как есть.
// b и i приводим к strong и em: редакторы пишут то одно, то другое, а
// хранить два написания одного и того же — значит потом сравнивать их в
// каждом тесте.
var rename = map[string]string{"b": "strong", "i": "em"}

var allowed = map[string]bool{
	"p": true, "br": true,
	"strong": true, "em": true, "u": true, "s": true,
	"ul": true, "ol": true, "li": true,
	"a": true, "span": true,
}

// void — теги без содержимого, закрывающий не пишем.
var void = map[string]bool{"br": true}

// dropWithContent — эти выкидываем вместе с содержимым. Для остальных
// незнакомых тегов содержимое сохраняем (тег разворачивается), но текст
// внутри <script> — это код, а не текст, и показывать его незачем.
var dropWithContent = map[string]bool{
	"script": true, "style": true, "iframe": true, "object": true,
	"embed": true, "template": true, "noscript": true, "svg": true,
	"math": true, "head": true, "title": true, "textarea": true,
}

// allowedScheme — ссылки только туда, откуда браузер не выполнит код.
// javascript:, data: и vbscript: отсекаются именно здесь.
func allowedScheme(href string) bool {
	h := strings.TrimSpace(href)
	// Схему ищем до первого ':' и только если он раньше '/', '?' и '#' —
	// иначе "/path:with:colon" ошибочно сочтётся схемой.
	for i, r := range h {
		switch r {
		case ':':
			scheme := strings.ToLower(h[:i])
			return scheme == "http" || scheme == "https" || scheme == "mailto" || scheme == "tel"
		case '/', '?', '#':
			return true // относительная ссылка
		}
	}
	return true // без схемы вовсе
}

// Sanitize разбирает размеченное сообщение и собирает из него безопасную
// разметку. mentionAllowed решает, можно ли упомянуть пользователя: ветку
// переписки видят не все, и упоминание постороннего — это способ дёрнуть
// уведомлением кого угодно. Не прошедшее проверку упоминание не исчезает
// вместе с текстом, а разворачивается: имя в сообщении остаётся, записи
// в comment_mentions не появляется. nil — упоминания не разрешены вовсе.
func Sanitize(raw string, mentionAllowed func(uuid.UUID) bool) (Result, error) {
	if len(raw) > maxInput {
		return Result{}, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(raw))
	}
	nodes, err := html.ParseFragment(strings.NewReader(raw), &html.Node{
		Type:     html.ElementNode,
		Data:     "body",
		DataAtom: atom.Body,
	})
	if err != nil {
		return Result{}, fmt.Errorf("richtext: parse: %w", err)
	}

	w := &writer{mentionAllowed: mentionAllowed, seen: map[uuid.UUID]bool{}}
	for _, n := range nodes {
		w.walk(n, 0)
	}

	res := Result{
		HTML:     strings.TrimSpace(w.html.String()),
		Text:     collapse(w.text.String()),
		Mentions: w.mentions,
	}
	if res.Text == "" {
		return Result{}, ErrEmpty
	}
	return res, nil
}

type writer struct {
	html           strings.Builder
	text           strings.Builder
	mentions       []uuid.UUID
	seen           map[uuid.UUID]bool
	mentionAllowed func(uuid.UUID) bool
}

func (w *writer) walk(n *html.Node, depth int) {
	switch n.Type {
	case html.TextNode:
		w.html.WriteString(html.EscapeString(n.Data))
		w.text.WriteString(n.Data)
		return
	case html.ElementNode:
		// продолжаем ниже
	default:
		// Комментарии, doctype и прочее в переписке не нужны.
		return
	}

	tag := strings.ToLower(n.Data)
	if dropWithContent[tag] {
		return
	}
	if r, ok := rename[tag]; ok {
		tag = r
	}
	// Слишком глубоко или тег незнакомый — разворачиваем: сам тег не пишем,
	// содержимое обходим.
	if depth >= maxDepth || !allowed[tag] {
		w.children(n, depth+1)
		return
	}

	if tag == "span" {
		w.span(n, depth)
		return
	}

	attrs := ""
	if tag == "a" {
		href := attr(n, "href")
		if !allowedScheme(href) {
			// Ссылка с опасной схемой перестаёт быть ссылкой, но текст
			// её остаётся: удалять написанное пользователем не наше дело.
			w.children(n, depth+1)
			return
		}
		// rel обязателен: без noopener открытая в новой вкладке страница
		// получает доступ к window.opener нашей.
		attrs = fmt.Sprintf(` href="%s" target="_blank" rel="nofollow noopener noreferrer"`,
			html.EscapeString(strings.TrimSpace(href)))
	}

	w.html.WriteString("<" + tag + attrs + ">")
	if void[tag] {
		w.text.WriteString("\n")
		return
	}
	if tag == "li" {
		w.text.WriteString("• ")
	}
	w.children(n, depth+1)
	w.html.WriteString("</" + tag + ">")
	if tag == "p" || tag == "li" {
		w.text.WriteString("\n")
	}
}

// span оставляем только как упоминание: обычный <span> в переписке носит
// оформление из чужого редактора и нам не нужен.
func (w *writer) span(n *html.Node, depth int) {
	id, err := uuid.Parse(strings.TrimSpace(attr(n, MentionAttr)))
	ok := err == nil && w.mentionAllowed != nil && w.mentionAllowed(id)
	if !ok {
		w.children(n, depth+1)
		return
	}
	if !w.seen[id] {
		w.seen[id] = true
		w.mentions = append(w.mentions, id)
	}
	w.html.WriteString(`<span ` + MentionAttr + `="` + id.String() + `">`)
	w.children(n, depth+1)
	w.html.WriteString("</span>")
}

func (w *writer) children(n *html.Node, depth int) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walk(c, depth)
	}
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val
		}
	}
	return ""
}

// collapse приводит текстовую проекцию к виду, пригодному для показа одной
// строкой: пробельные последовательности схлопываются, пустые строки
// выбрасываются.
func collapse(s string) string {
	var b strings.Builder
	lastSpace, lastNL := true, true
	for _, r := range s {
		switch {
		case r == '\n':
			if !lastNL {
				b.WriteRune('\n')
			}
			lastNL, lastSpace = true, true
		case r == ' ' || r == '\t' || r == '\r' || r == '\v' || r == '\f' || r == ' ':
			if !lastSpace {
				b.WriteRune(' ')
			}
			lastSpace = true
		default:
			b.WriteRune(r)
			lastSpace, lastNL = false, false
		}
	}
	return strings.TrimSpace(b.String())
}

// PlainText — путь для body_format='plain': разметки нет, но текстовая
// проекция нужна такая же, как у html-ветки.
func PlainText(raw string) string { return collapse(raw) }
