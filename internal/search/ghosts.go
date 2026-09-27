package search

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// Призраки в индексе: документы людей, которых в базе больше нет.
//
// Источник истины для поиска — OpenSearch, и попадает туда всё через
// outbox: удалили человека приложением — событие есть, документ уходит.
// Но пользователей удаляют и МИМО приложения: харнесс интеграционных
// тестов и фикстуры e2e сносят своих людей одним `DELETE FROM users`,
// потому что ходить ради уборки через HTTP им незачем. Outbox при этом
// не пишется, и документ живёт в индексе вечно.
//
// Цена этого не «лишняя строка в выдаче». Заказчик отмечает такого
// человека в воронке «под ключ», жмёт «Отправить» и получает 409
// not_a_creator: «в пакет берутся только блогеры и авторы UGC». Он
// видел карточку с именем и фотографией — и не может понять, почему
// система сначала его предложила, а потом отказала. Это случалось уже
// дважды, оба раза чинилось полным реиндексом руками, и оба раза
// возвращалось со следующим прогоном тестов.
//
// Поэтому уборка стала фоновой задачей воркера: индекс сверяется с
// базой, и документ, за которым нет пользователя, удаляется.
//
// Осторожность здесь важнее полноты. Удаляем ТОЛЬКО тех, у кого в
// `users` нет строки вовсе. «Снялся с публикации», «на модерации»,
// «профиль не заполнен» — это состояния живого человека, и за ними
// приходит своё событие; спутать их с призраком значит выкосить из
// каталога половину живых.

// ghostScanLimit — сколько документов смотрим за проход.
//
// Одним запросом и без скролла: на объёме MVP это сотни документов, а
// не миллионы. Упрётся в потолок — следующий проход доберёт остальное,
// и это честнее, чем сложная пагинация ради задачи, которая обычно
// находит ноль.
const ghostScanLimit = 1000

// SweepGhosts — убрать из индекса документы людей, которых нет в базе.
//
// Возвращает, сколько удалено. Ноль — нормальный и ожидаемый ответ.
func (i *Indexer) SweepGhosts(ctx context.Context) (int, error) {
	resp, err := i.es.Search(ctx, i.index, map[string]any{
		"size":    ghostScanLimit,
		"_source": false,
		"query":   map[string]any{"match_all": map[string]any{}},
	})
	if err != nil {
		return 0, fmt.Errorf("scan index: %w", err)
	}
	ids := make([]uuid.UUID, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		id, err := uuid.Parse(h.ID)
		if err != nil {
			// Чужой документ с нечитаемым id: не наш и не призрак —
			// удалять то, чего мы не понимаем, нельзя.
			slog.Warn("index ghost sweep: unparsable doc id", "id", h.ID)
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	alive, err := i.repo.ExistingUserIDs(ctx, ids)
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, id := range ids {
		if alive[id] {
			continue
		}
		// version=0 — без проверки версии: спорить не с кем, строки
		// пользователя нет, и более свежей правды про него не будет.
		if err := i.es.DeleteDoc(ctx, i.index, id.String()); err != nil {
			slog.Warn("index ghost sweep: delete failed", "user_id", id, "err", err)
			continue
		}
		slog.Info("index ghost removed", "user_id", id, "index", i.index)
		removed++
	}
	return removed, nil
}

// SweepGhosts — то же для индекса роликов: у удалённого человека
// остаются и его видео, и они так же попадают в выдачу.
func (i *FeedIndexer) SweepGhosts(ctx context.Context) (int, error) {
	resp, err := i.es.Search(ctx, i.index, map[string]any{
		"size": 0,
		"aggs": map[string]any{
			"users": map[string]any{
				"terms": map[string]any{"field": "user_id", "size": ghostScanLimit},
			},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("scan feed index: %w", err)
	}
	ids, err := aggUserIDs(resp.Aggregations)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	alive, err := i.repo.ExistingUserIDs(ctx, ids)
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, id := range ids {
		if alive[id] {
			continue
		}
		if err := i.DeleteByUser(ctx, id); err != nil {
			slog.Warn("feed ghost sweep: delete failed", "user_id", id, "err", err)
			continue
		}
		slog.Info("feed ghosts removed", "user_id", id)
		removed++
	}
	return removed, nil
}

// aggUserIDs — разобрать terms-агрегацию по user_id.
//
// Своим разбором, а не типизированным ответом клиента: агрегации
// приходят сырым JSON, и заводить общий тип ради одного места значит
// описывать чужую схему целиком.
func aggUserIDs(raw []byte) ([]uuid.UUID, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var parsed struct {
		Users struct {
			Buckets []struct {
				Key string `json:"key"`
			} `json:"buckets"`
		} `json:"users"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse users agg: %w", err)
	}
	out := make([]uuid.UUID, 0, len(parsed.Users.Buckets))
	for _, b := range parsed.Users.Buckets {
		id, err := uuid.Parse(b.Key)
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}
