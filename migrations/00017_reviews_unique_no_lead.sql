-- +goose Up
-- data-sec D10: партиальный UNIQUE поверх (author_user_id, target_user_id)
-- ТОЛЬКО для отзывов без lead_id. Полный UNIQUE(lead_id, author, target)
-- из 00001 не покрывает NULL'ы (PG считает NULL отличными), поэтому
-- любой авторизованный мог фармить десятки 1-звёздных отзывов конкуренту,
-- не прикреплённых к лидам.
--
-- Сценарий который теперь блокируется: атакующий создаёт N аккаунтов и
-- шлёт POST /reviews без lead_id целевому спецу. До этого ограничения
-- ничего не отстреливало; теперь второй INSERT от того же автора падает
-- с 23505 → handler возвращает 409 review_exists.

-- Сначала чистим уже накопившиеся дубли: оставляем самый свежий отзыв
-- по (author, target) среди тех что без lead_id; остальные удаляем.
-- Триггер reviews_recalc_trg сам пересчитает rating_avg/count.
DELETE FROM reviews r
WHERE r.lead_id IS NULL
  AND r.id NOT IN (
    SELECT DISTINCT ON (author_user_id, target_user_id) id
    FROM reviews
    WHERE lead_id IS NULL
    ORDER BY author_user_id, target_user_id, created_at DESC
  );

CREATE UNIQUE INDEX IF NOT EXISTS reviews_author_target_no_lead_uniq
  ON reviews (author_user_id, target_user_id)
  WHERE lead_id IS NULL;

-- +goose Down
DROP INDEX IF EXISTS reviews_author_target_no_lead_uniq;
