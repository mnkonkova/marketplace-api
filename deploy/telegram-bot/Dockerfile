# Сервис двух телеграм-ботов. Зависимостей нет вовсе — только Node 20+
# со встроенным fetch, — поэтому и слоя установки нет.
FROM node:20-alpine
WORKDIR /app
COPY package.json ./
COPY src ./src
ENV NODE_ENV=production
EXPOSE 8080
CMD ["node", "src/server.js"]
