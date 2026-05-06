# syntax=docker/dockerfile:1.7
# Multi-stage Go build для api/worker/seed (один образ, разные команды)
# и goose-CLI для миграций. CGO выключен — статический бинарь, мелкий
# alpine-runtime. styles.css ожидается уже собранным в репо (см. Makefile
# build-css).

FROM golang:1.22-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/api    ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/seed   ./cmd/seed && \
    CGO_ENABLED=0 GOOS=linux go install github.com/pressly/goose/v3/cmd/goose@latest

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S app && adduser -S -G app app

WORKDIR /app
COPY --from=build /out/api    /usr/local/bin/api
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/seed   /usr/local/bin/seed
COPY --from=build /go/bin/goose /usr/local/bin/goose
COPY --chown=app:app web/        ./web/
COPY --chown=app:app migrations/ ./migrations/

USER app
ENV WEB_DIR=/app/web
EXPOSE 8080
CMD ["api"]
