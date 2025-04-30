FROM python:3.9-slim

# Установка рабочей директории
WORKDIR /app

# Установка зависимостей
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Установка ffmpeg для работы с аудио через pydub
RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg \
    gosu \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Копирование файлов приложения
COPY . .

# Создание стандартных директорий для аудиофайлов
RUN mkdir -p /app/audio /app/humor /app/science && \
    chmod 777 /app/audio /app/humor /app/science

# Создаем пользователя с низкими привилегиями
RUN groupadd -r appuser && useradd -r -g appuser appuser

# Открытие порта
EXPOSE 9999

# Переменные среды для Sentry
ENV ENVIRONMENT=production
ENV RELEASE=1.0.0

# Аргумент сборки для настройки маршрутов аудиопотока
ARG AUDIO_STREAM_ROUTES=/,/humor,/science
ENV AUDIO_STREAM_ROUTES=${AUDIO_STREAM_ROUTES}

# Аргумент сборки для настройки сопоставления директорий и маршрутов
ARG DIRECTORY_ROUTES='{"humor":"/app/humor","science":"/app/science"}'
ENV DIRECTORY_ROUTES=${DIRECTORY_ROUTES}

# Скрипт-обертка для запуска с правильными правами доступа
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Точка входа в контейнер
ENTRYPOINT ["/entrypoint.sh"]

# Запуск приложения с директорией аудио по умолчанию
CMD ["python", "main.py", "--audio-dir", "/app/audio"] 