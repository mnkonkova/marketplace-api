package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Проверка ролика менеджером.
//
// Чек-лист до сих пор был односторонним: креатор отмечал пункты сам, и
// «я сделал» было единственным утверждением в системе. Здесь появляется
// второе — «я проверил», — и вся польза в том, что они могут разойтись:
// креатор отметил логотип, менеджер посмотрел и логотипа не увидел.
//
// Главное правило, ради которого тест и написан: принять ролик нельзя,
// пока обязательный пункт не отмечен «да» ИМЕННО менеджером. Отметка
// креатора здесь не считается — иначе проверка проверяла бы сама себя.
func TestPublicationReview(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()
	creator, manager := creators[0], creators[1]

	svc := publications.NewService(publications.NewRepo(pool))

	// Два пункта: обязательный общий и необязательный. Необязательный
	// нужен, чтобы «принять» не требовало его закрытия.
	var mustItem, mayItem uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO project_checklist_items (project_id, text, is_required, sort_order)
VALUES ($1, 'Логотип в первые 3 секунды', TRUE, 0) RETURNING id`, projectID).Scan(&mustItem); err != nil {
		t.Fatalf("seed required item: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO project_checklist_items (project_id, text, is_required, sort_order)
VALUES ($1, 'Шрифт титров — Onest Bold', FALSE, 1) RETURNING id`, projectID).Scan(&mayItem); err != nil {
		t.Fatalf("seed optional item: %v", err)
	}

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creator},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      manager,
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID

	t.Run("смотреть нечего, пока нет ссылок", func(t *testing.T) {
		_, err := svc.Review(ctx, publications.ReviewInput{
			PublicationID: pubID,
			ManagerUserID: manager,
			Decision:      publications.DecisionAccept,
		})
		if !errors.Is(err, publications.ErrNothingToReview) {
			t.Fatalf("ожидали ErrNothingToReview, получили %v", err)
		}
	})

	// Креатор сдаёт ролик и сам отмечает обязательный пункт.
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID:  pubID,
		ActorUserID:    creator,
		URLs:           []string{"https://www.tiktok.com/@u/video/111"},
		CheckedItemIDs: []uuid.UUID{mustItem},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	t.Run("отметка креатора не заменяет проверку", func(t *testing.T) {
		_, err := svc.Review(ctx, publications.ReviewInput{
			PublicationID: pubID,
			ManagerUserID: manager,
			Decision:      publications.DecisionAccept,
		})
		if !errors.Is(err, publications.ErrReviewBlocked) {
			t.Fatalf("ожидали ErrReviewBlocked, получили %v", err)
		}
	})

	t.Run("ход проверки сохраняется без решения", func(t *testing.T) {
		got, err := svc.Review(ctx, publications.ReviewInput{
			PublicationID: pubID,
			ManagerUserID: manager,
			Marks:         []publications.ReviewMark{{ItemID: mayItem, Passed: true}},
		})
		if err != nil {
			t.Fatalf("Review: %v", err)
		}
		if got.Review == nil || got.Review.Status != publications.ReviewInReview {
			t.Fatalf("ожидали статус in_review, получили %+v", got.Review)
		}
		if got.Review.DecidedAt != nil {
			t.Fatalf("решения не выносили, а decided_at проставлен: %+v", got.Review)
		}
		if len(got.Review.Marks) != 1 {
			t.Fatalf("ожидали один вердикт, получили %+v", got.Review.Marks)
		}
	})

	t.Run("возврат без замечания не принимается", func(t *testing.T) {
		_, err := svc.Review(ctx, publications.ReviewInput{
			PublicationID: pubID,
			ManagerUserID: manager,
			Marks:         []publications.ReviewMark{{ItemID: mustItem, Passed: false}},
			Decision:      publications.DecisionReturn,
		})
		if !errors.Is(err, publications.ErrInvalidInput) {
			t.Fatalf("ожидали ErrInvalidInput, получили %v", err)
		}
	})

	t.Run("возврат с замечанием", func(t *testing.T) {
		got, err := svc.Review(ctx, publications.ReviewInput{
			PublicationID: pubID,
			ManagerUserID: manager,
			Marks:         []publications.ReviewMark{{ItemID: mustItem, Passed: false}},
			Comment:       "Логотипа не видно, он появляется на пятой секунде.",
			Decision:      publications.DecisionReturn,
		})
		if err != nil {
			t.Fatalf("Review: %v", err)
		}
		if got.Review.Status != publications.ReviewReturned {
			t.Fatalf("ожидали returned, получили %q", got.Review.Status)
		}
		if got.Review.Comment == "" || got.Review.DecidedAt == nil {
			t.Fatalf("возврат без замечания или без отметки времени: %+v", got.Review)
		}
		if len(got.Review.Failed()) != 1 {
			t.Fatalf("ожидали один непройденный пункт, получили %+v", got.Review.Marks)
		}
		// Вердикт «нет» — это не отсутствие отметки: строка в базе есть.
		var passed bool
		if err := pool.QueryRow(ctx, `
SELECT passed FROM publication_review_marks WHERE publication_id = $1 AND item_id = $2`,
			pubID, mustItem).Scan(&passed); err != nil {
			t.Fatalf("read mark: %v", err)
		}
		if passed {
			t.Fatalf("сохранили «да» там, где менеджер поставил «нет»")
		}
	})

	t.Run("пункт с вердиктом из чек-листа не выкидывается", func(t *testing.T) {
		err := svc.DeleteChecklistItem(ctx, projectID, mustItem)
		if !errors.Is(err, publications.ErrChecklistItemUsed) {
			t.Fatalf("ожидали ErrChecklistItemUsed, получили %v", err)
		}
	})

	t.Run("пересдача отменяет прежнее решение", func(t *testing.T) {
		if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
			PublicationID:  pubID,
			ActorUserID:    creator,
			URLs:           []string{"https://vk.com/clip-1_456239017"},
			CheckedItemIDs: []uuid.UUID{mustItem},
		}); err != nil {
			t.Fatalf("SubmitLinks: %v", err)
		}
		got, err := svc.Get(ctx, pubID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Review.Status != publications.ReviewInReview {
			t.Fatalf("после пересдачи ожидали in_review, получили %q", got.Review.Status)
		}
		// Замечание относилось к прежнему ролику — показывать его как
		// действующую претензию нельзя.
		if got.Review.Comment != "" {
			t.Fatalf("замечание пережило пересдачу: %q", got.Review.Comment)
		}
		if len(got.Review.Marks) != 0 {
			t.Fatalf("вердикты пережили пересдачу: %+v", got.Review.Marks)
		}
		if got.Review.Round != 2 {
			t.Fatalf("ожидали вторую сдачу, получили round=%d", got.Review.Round)
		}
	})

	t.Run("принять можно, когда обязательные пройдены", func(t *testing.T) {
		got, err := svc.Review(ctx, publications.ReviewInput{
			PublicationID: pubID,
			ManagerUserID: manager,
			Marks:         []publications.ReviewMark{{ItemID: mustItem, Passed: true}},
			Decision:      publications.DecisionAccept,
		})
		if err != nil {
			t.Fatalf("Review: %v", err)
		}
		if got.Review.Status != publications.ReviewAccepted {
			t.Fatalf("ожидали accepted, получили %q", got.Review.Status)
		}
		if got.Review.DecidedBy == nil || *got.Review.DecidedBy != manager {
			t.Fatalf("решение безымянное: %+v", got.Review)
		}
	})

	t.Run("вердикт по чужому пункту не проходит", func(t *testing.T) {
		_, err := svc.Review(ctx, publications.ReviewInput{
			PublicationID: pubID,
			ManagerUserID: manager,
			Marks:         []publications.ReviewMark{{ItemID: uuid.New(), Passed: true}},
		})
		if !errors.Is(err, publications.ErrForeignChecklistItem) {
			t.Fatalf("ожидали ErrForeignChecklistItem, получили %v", err)
		}
	})
}

// «Я исправил — проверьте ещё раз», не трогая ссылки.
//
// Возврат на доработку был тупиком. Менеджер возвращает ролик со
// словами «нет ссылки в шапке профиля», креатор правит это НА ПЛОЩАДКЕ,
// и адрес ролика при этом не меняется. Сказать сервису «готово» ему
// было нечем: окно сдачи требует НОВУЮ ссылку, а замена ссылки тем же
// адресом проверку не переоткрывает — переоткрытие висит на смене
// адреса, потому что другой адрес значит другой ролик.
//
// Оставалось два выхода, оба плохие: написать менеджеру в переписку
// (работа уходит из сервиса) или перезалить ролик ради нового адреса,
// потеряв набранные просмотры.
func TestCreatorResubmitsReturnedPublication(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators[:1],
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID

	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://www.tiktok.com/@u/video/4242"},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	// Пока ролик на проверке, отправлять его туда же нечего.
	if _, err := svc.CreatorResubmit(ctx, pubID, creators[0]); !errors.Is(
		err, publications.ErrNothingToResubmit) {
		t.Errorf("пересдача открытой проверки: %v, ожидалось ErrNothingToResubmit", err)
	}

	if _, err := svc.Review(ctx, publications.ReviewInput{
		PublicationID: pubID,
		ManagerUserID: creators[0],
		Decision:      publications.DecisionReturn,
		Comment:       "нет ссылки в шапке профиля",
	}); err != nil {
		t.Fatalf("Review: %v", err)
	}

	got, err := svc.CreatorResubmit(ctx, pubID, creators[0])
	if err != nil {
		t.Fatalf("CreatorResubmit: %v", err)
	}
	if got.Review == nil || got.Review.Status != publications.ReviewInReview {
		t.Fatalf("проверка не открылась заново: %+v", got.Review)
	}
	if got.Review.Round != 2 {
		t.Errorf("круг проверки %d, ожидался второй", got.Review.Round)
	}
	// Ссылка осталась та же: ролик тот же, изменилось то, что в нём
	// исправили. Перезаливать ради нового адреса — терять просмотры.
	if len(got.Links) != 1 {
		t.Fatalf("ссылок %d, ожидалась одна — пересдача их не трогает", len(got.Links))
	}

	// Чужой ролик так не пересдать, и ответ — «не найдено»: подтверждать
	// постороннему существование чужой выкладки незачем.
	if _, err := svc.CreatorResubmit(ctx, pubID, creators[1]); !errors.Is(
		err, publications.ErrForbidden) {
		t.Errorf("чужая пересдача: %v, ожидалось ErrForbidden", err)
	}
}
