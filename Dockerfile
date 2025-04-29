FROM python:3.9-slim

# Установка рабочей директории
WORKDIR /app

# Установка зависимостей
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Установка ffmpeg для работы с аудио через pydub
RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Копирование файлов приложения
COPY . .

# Создание директории для аудиофайлов 
# (можно примонтировать внешний volume с аудиофайлами)
RUN mkdir -p /app/audio

# Открытие порта
EXPOSE 9999

# Запуск приложения с директорией аудио
CMD ["python", "main.py", "--audio-dir", "/app/audio"] 