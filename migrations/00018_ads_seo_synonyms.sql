-- +goose Up
-- +goose StatementBegin
-- Категория ads_seo раньше называлась «Таргет + SEO». Имя с «+» сбивало LLM:
-- на запросы вида «нужен таргетолог» / «запусти таргет» clarify не маркировал
-- её, потому что трактовал «+» как «требуется И таргет, И SEO». Плюс описание
-- было сухое («Настройка таргетированной рекламы и SEO-продвижение») — без
-- синонимов, по которым LLM матчит свободный текст.
--
-- Здесь — расширяем title (явное «или») и описание (синонимы таргет/
-- таргетолог/performance + список платформ + контекстная реклама).
UPDATE specialty_categories
SET
  title       = 'Таргетолог / SEO',
  description = 'Таргетированная реклама (таргет, таргетолог, performance marketing), контекстная реклама и SEO. Запуск кампаний в Meta Ads, ВК Реклама, Яндекс Директ, Google Ads, MyTarget. Технические специалисты, не креативные.'
WHERE code = 'ads_seo';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
UPDATE specialty_categories
SET
  title       = 'Таргет + SEO',
  description = 'Настройка таргетированной рекламы и SEO-продвижение'
WHERE code = 'ads_seo';
-- +goose StatementEnd
