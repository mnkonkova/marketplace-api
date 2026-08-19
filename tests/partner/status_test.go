// Состояние профиля глазами «Бота Работ». Партнёрская цена там даётся за
// опубликованный профиль, и от этого ответа зависит, что человеку скажут
// делать: заполнять анкету, ждать модерацию, исправлять замечания или
// возвращать публикацию. Одно «нет в каталоге» на все четыре случая — это
// совет невпопад тому, кто уже всё сделал и ждёт.
package partner_test

import (
	"testing"

	"marketpclce/internal/partner"
)

func TestSpecialistStateTellsCasesApart(t *testing.T) {
	cases := []struct {
		name       string
		published  bool
		moderation string
		want       string
	}{
		{"опубликован и одобрен", true, "approved", partner.StatusPublished},
		{"ждёт модерации", false, "pending_review", partner.StatusPending},
		{"отклонён", false, "rejected", partner.StatusRejected},
		{"одобрен, но снят с публикации", false, "approved", partner.StatusHidden},
		// Отклонённый с флагом публикации: в каталог такой не попадает
		// (фильтр требует и approved, и is_published), и партнёру важна
		// именно причина, а не флаг.
		{"отклонён при поднятом флаге", true, "rejected", partner.StatusRejected},
		{"ждёт модерации при поднятом флаге", true, "pending_review", partner.StatusPending},
	}
	for _, c := range cases {
		if got := partner.SpecialistState(c.published, c.moderation); got != c.want {
			t.Errorf("%s: получили %q, ждали %q", c.name, got, c.want)
		}
	}
}

func TestSecretIsCheckedAndNotGuessable(t *testing.T) {
	svc := partner.NewService(nil, "https://bot/hook", "s3cret")

	if !svc.CheckSecret("s3cret") {
		t.Error("верный секрет должен приниматься")
	}
	for _, wrong := range []string{"", "s3cre", "s3cret ", "S3CRET", "другое"} {
		if svc.CheckSecret(wrong) {
			t.Errorf("секрет %q принят, а не должен", wrong)
		}
	}

	// Без секрета связка выключена целиком: половина механизма опаснее его
	// отсутствия — ручка отдавала бы статусы модерации кому угодно.
	off := partner.NewService(nil, "https://bot/hook", "")
	if off.CheckSecret("") || off.CheckSecret("что-нибудь") {
		t.Error("без общего секрета не должно пускать вообще")
	}
}
