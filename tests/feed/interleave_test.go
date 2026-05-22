package feed_test

import (
	"strings"
	"testing"

	"marketpclce/internal/feed"
	"marketpclce/internal/search"
)

// docs хелпер: строит []FeedVideoDoc из строки "A1,A2,B1,A3" (UserID =
// первая буква, VideoID = всё). Сохраняет порядок ES-выдачи.
func docs(spec string) []search.FeedVideoDoc {
	parts := strings.Split(spec, ",")
	out := make([]search.FeedVideoDoc, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, search.FeedVideoDoc{
			VideoID: p,
			UserID:  string(p[0]),
		})
	}
	return out
}

func videoIDs(ds []search.FeedVideoDoc) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.VideoID
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestInterleaveByUserEmpty(t *testing.T) {
	got := feed.InterleaveByUser(nil)
	if len(got) != 0 {
		t.Errorf("ожидаем пустой выход для nil-входа, got %v", got)
	}
}

func TestInterleaveByUserSingle(t *testing.T) {
	in := docs("A1")
	got := feed.InterleaveByUser(in)
	if !equal(videoIDs(got), []string{"A1"}) {
		t.Errorf("один элемент должен пройти как есть, got %v", videoIDs(got))
	}
}

// «Идеальный» кейс: видео всех пользователей чередуются 1-к-1 — результат
// должен совпадать с входом (round-robin не должен ничего ломать).
func TestInterleaveByUserAlreadyAlternating(t *testing.T) {
	in := docs("A1,B1,C1,A2,B2,C2")
	got := feed.InterleaveByUser(in)
	want := []string{"A1", "B1", "C1", "A2", "B2", "C2"}
	if !equal(videoIDs(got), want) {
		t.Errorf("got %v, want %v", videoIDs(got), want)
	}
}

// Кластер одного юзера в начале: ES вернул A1,A2,A3,B1 (у A топ-3 видео).
// После interleave должно быть A1,B1,A2,A3 — B1 поднимается на 2-ю позицию,
// остальные A — за ним. Это и есть diversity: спецу A не дают 3 строки подряд.
func TestInterleaveByUserClusteredHead(t *testing.T) {
	in := docs("A1,A2,A3,B1")
	got := feed.InterleaveByUser(in)
	want := []string{"A1", "B1", "A2", "A3"}
	if !equal(videoIDs(got), want) {
		t.Errorf("got %v, want %v", videoIDs(got), want)
	}
}

// Внутри одного юзера ES-порядок должен сохраняться (sort.SliceStable
// эквивалент). Если у A видео в порядке A1,A2,A3 — в выдаче они тоже
// должны идти в этом относительном порядке.
func TestInterleaveByUserPreservesOrderWithinUser(t *testing.T) {
	in := docs("A1,B1,A2,B2,A3")
	got := feed.InterleaveByUser(in)
	// Из interleave-алгоритма (round-robin по юзерам в порядке появления):
	//   round 0: A1, B1
	//   round 1: A2, B2
	//   round 2: A3
	want := []string{"A1", "B1", "A2", "B2", "A3"}
	if !equal(videoIDs(got), want) {
		t.Errorf("got %v, want %v", videoIDs(got), want)
	}
}

// Один доминирующий юзер: A1..A5, B1, C1. Должны попасть на 2-ю и 3-ю позиции
// чтобы разбавить кластер A.
func TestInterleaveByUserSingleDominantUser(t *testing.T) {
	in := docs("A1,A2,A3,A4,A5,B1,C1")
	got := feed.InterleaveByUser(in)
	want := []string{"A1", "B1", "C1", "A2", "A3", "A4", "A5"}
	if !equal(videoIDs(got), want) {
		t.Errorf("got %v, want %v", videoIDs(got), want)
	}
}

// Количество элементов после interleave должно совпадать с входом
// (никого не теряем и не добавляем).
func TestInterleaveByUserCountPreserved(t *testing.T) {
	in := docs("A1,A2,B1,C1,C2,C3,D1")
	got := feed.InterleaveByUser(in)
	if len(got) != len(in) {
		t.Errorf("после interleave должно быть %d элементов, got %d", len(in), len(got))
	}
}
