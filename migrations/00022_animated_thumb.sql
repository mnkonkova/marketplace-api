-- +goose Up
-- Animated WebP «гифка» для autoplay на главной (hero + works grid).
-- Решает iOS Low Power Mode + соncurrent <video> лимит: <img> с
-- animated webp не блокируется ни LPM, ни soft-limit'ом на 4-8 видео.
-- См. docs/VIDEO_TRANSCODING.md §11.
--
-- Генерится тем же transcode-worker'ом что и preview_url (sequential
-- pass после mp4). Если webp-pass упал — animated_thumb_url остаётся
-- NULL, фронт фолбэчит на <video preview_url>.
ALTER TABLE portfolio_items
    ADD COLUMN animated_thumb_url TEXT;

-- +goose Down
ALTER TABLE portfolio_items
    DROP COLUMN animated_thumb_url;
