# Этап 1: Сборка приложения
FROM golang:1.22-alpine AS builder

# Установка зависимостей для сборки
RUN apk add --no-cache git

# Создание рабочей директории
WORKDIR /app

# Копирование только go.mod сначала
COPY go.mod ./

# Выполнение go mod tidy для создания/обновления go.sum
RUN go mod tidy

# Загрузка всех зависимостей (явно)
RUN go mod download all

# Копирование исходного кода
COPY . .

# Повторное выполнение go mod tidy после копирования кода
RUN go mod tidy && go mod verify

# Сборка приложения
# Флаг CGO_ENABLED=0 для статической компиляции
# Флаг -ldflags="-s -w" для оптимизации размера бинарного файла
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o audio-streamer .

# Этап 2: Создание итогового образа
FROM alpine:latest

# Установка базовых утилит
RUN apk add --no-cache ca-certificates tzdata curl findutils

# Копирование бинарного файла из этапа сборки
COPY --from=builder /app/audio-streamer /app/
COPY --from=builder /app/web /app/web
COPY --from=builder /app/templates /app/templates
COPY --from=builder /app/image /app/image

# Копирование entrypoint-скрипта
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Создание директории для аудиофайлов
RUN mkdir -p /app/audio /app/humor /app/science && \
    chmod -R 755 /app

# Рабочая директория
WORKDIR /app

# Открытие порта
EXPOSE 8000

# Улучшенный HEALTHCHECK - использовать метрики, которые доступны сразу
HEALTHCHECK --interval=30s --timeout=2s --start-period=5s --retries=3 \
  CMD wget -q -t1 -T2 http://localhost:8000/metrics || exit 1

# Точка входа - наш entrypoint-скрипт
ENTRYPOINT ["/app/entrypoint.sh"]

# Аргументы по умолчанию - запуск аудио-стримера без явного указания директории
CMD ["/app/audio-streamer"] 