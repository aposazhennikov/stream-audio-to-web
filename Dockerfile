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

# Создание директорий для аудиофайлов 
# (можно примонтировать внешние volumes с аудиофайлами)
RUN mkdir -p /app/audio && chmod 777 /app/audio
RUN mkdir -p /app/humor && chmod 777 /app/humor
RUN mkdir -p /app/science && chmod 777 /app/science
RUN mkdir -p /app/business && chmod 777 /app/business

# Создаем пользователя с низкими привилегиями
RUN groupadd -r appuser && useradd -r -g appuser appuser

# Открытие порта
EXPOSE 9999

# Переменные среды для Sentry
ENV ENVIRONMENT=production
ENV RELEASE=1.0.0

# Аргумент сборки для настройки маршрутов аудиопотока (для обратной совместимости)
ARG AUDIO_STREAM_ROUTES=/
# Устанавливаем переменную окружения из аргумента сборки
ENV AUDIO_STREAM_ROUTES=${AUDIO_STREAM_ROUTES}

# Аргумент сборки для карты папок и маршрутов
ARG FOLDER_ROUTE_MAP=/app/humor:/humor,/app/science:/science,/app/business:/business,/app/audio:/
# Устанавливаем переменную окружения из аргумента сборки
ENV FOLDER_ROUTE_MAP=${FOLDER_ROUTE_MAP}

# Скрипт-обертка для запуска с правильными правами доступа
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Точка входа в контейнер
ENTRYPOINT ["/entrypoint.sh"]

# Запуск приложения
CMD ["python", "main.py"] 