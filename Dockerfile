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

# Создание директории для аудиофайлов 
# (можно примонтировать внешний volume с аудиофайлами)
RUN mkdir -p /app/audio && chmod 777 /app/audio

# Создаем пользователя с низкими привилегиями
RUN groupadd -r appuser && useradd -r -g appuser appuser

# Открытие порта
EXPOSE 9999

# Переменные среды для Sentry
ENV ENVIRONMENT=production
ENV RELEASE=1.0.0

# Скрипт-обертка для запуска с правильными правами доступа
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Точка входа в контейнер
ENTRYPOINT ["/entrypoint.sh"]

# Запуск приложения с директорией аудио
CMD ["python", "main.py", "--audio-dir", "/app/audio"] 