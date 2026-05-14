-- +goose Up
-- +goose StatementBegin

-- Пересчёт rating_avg / reviews_count для одного специалиста. Дёргается
-- триггером после любого INSERT/UPDATE/DELETE на reviews. До этой
-- миграции колонки висели нулями, потому что ручка записи отзыва ничего
-- не апдейтила и бэкграунд-джобы тоже не было.
CREATE OR REPLACE FUNCTION reviews_recalc(p_target UUID) RETURNS VOID AS $$
BEGIN
  UPDATE specialist_profiles sp SET
    rating_avg    = COALESCE((SELECT ROUND(AVG(rating)::NUMERIC, 2) FROM reviews WHERE target_user_id = p_target), 0),
    reviews_count = (SELECT COUNT(*) FROM reviews WHERE target_user_id = p_target),
    updated_at    = now()
  WHERE sp.user_id = p_target;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION trg_reviews_after_change() RETURNS TRIGGER AS $$
BEGIN
  IF (TG_OP = 'DELETE') THEN
    PERFORM reviews_recalc(OLD.target_user_id);
  ELSIF (TG_OP = 'UPDATE') THEN
    PERFORM reviews_recalc(NEW.target_user_id);
    IF OLD.target_user_id <> NEW.target_user_id THEN
      PERFORM reviews_recalc(OLD.target_user_id);
    END IF;
  ELSE
    PERFORM reviews_recalc(NEW.target_user_id);
  END IF;
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER reviews_recalc_trg
AFTER INSERT OR UPDATE OR DELETE ON reviews
FOR EACH ROW EXECUTE FUNCTION trg_reviews_after_change();

-- Бэкфилл: если кто-то заливал отзывы напрямую в SQL до того, как
-- триггер встал, агрегаты у этих специалистов всё ещё нули.
UPDATE specialist_profiles sp SET
  rating_avg    = COALESCE(r.avg_rating, 0),
  reviews_count = COALESCE(r.cnt, 0)
FROM (
  SELECT target_user_id,
         ROUND(AVG(rating)::NUMERIC, 2) AS avg_rating,
         COUNT(*)                       AS cnt
  FROM reviews
  GROUP BY target_user_id
) r WHERE sp.user_id = r.target_user_id;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TRIGGER IF EXISTS reviews_recalc_trg ON reviews;
DROP FUNCTION IF EXISTS trg_reviews_after_change();
DROP FUNCTION IF EXISTS reviews_recalc(UUID);

-- +goose StatementEnd
