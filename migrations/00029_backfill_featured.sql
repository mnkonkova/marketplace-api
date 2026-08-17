-- +goose Up
-- +goose StatementBegin

-- Разовый бэкфилл: у всех, кто загрузил работы до появления is_featured,
-- закрепляем первую — иначе флагман на публичной странице не покажется
-- никому, пока каждый специалист вручную не выберет промо в кабинете
-- (а кабинетной кнопки ещё нет).
--
-- Это именно дефолт, а не «выбор за специалиста»: как только в /me
-- появится переключатель, он перекроет это значение обычным UPDATE'ом.
--
-- Порядок выбора: сначала видео (промо-ролик — основной кейс), при их
-- отсутствии — первая работа любого вида, чтобы фото-специалисты тоже
-- получили флагман. Внутри — по sort_order, то есть по тому порядку,
-- который специалист сам выставил в портфолио.
--
-- NOT EXISTS защищает от перетирания: если кто-то уже успел закрепить
-- работу через API, его выбор остаётся.
WITH first_work AS (
    SELECT DISTINCT ON (user_id) id, user_id
      FROM portfolio_items
     ORDER BY user_id,
              (kind = 'video' AND video_url IS NOT NULL AND video_url <> '') DESC,
              sort_order,
              created_at
)
UPDATE portfolio_items p
   SET is_featured = TRUE
  FROM first_work f
 WHERE p.id = f.id
   AND NOT EXISTS (
        SELECT 1
          FROM portfolio_items x
         WHERE x.user_id = p.user_id
           AND x.is_featured
   );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Откатить точно нельзя: после бэкфилла не отличить «поставили здесь» от
-- «специалист выбрал сам». Снимаем все закрепления — состояние до 00029
-- с точки зрения фичи (флагман просто не показывается).
UPDATE portfolio_items SET is_featured = FALSE WHERE is_featured;

-- +goose StatementEnd
