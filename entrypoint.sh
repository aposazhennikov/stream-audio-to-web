#!/bin/bash
set -e

# Получаем ID пользователя и группы владельца примонтированного volume
OWNER_UID=$(stat -c "%u" /app/audio)
OWNER_GID=$(stat -c "%g" /app/audio)

echo "Проверка владельца примонтированного каталога /app/audio: UID=$OWNER_UID, GID=$OWNER_GID"

# Если владелец не root (UID 0), настраиваем пользователя appuser на эти UID/GID
if [ "$OWNER_UID" != "0" ]; then
    echo "Настройка пользователя appuser на UID=$OWNER_UID, GID=$OWNER_GID"
    groupmod -o -g "$OWNER_GID" appuser
    usermod -o -u "$OWNER_UID" appuser
    
    # Проверяем наличие прав на чтение для всех файлов
    echo "Проверка прав доступа к файлам в /app/audio"
    find /app/audio -type f -name "*.mp3" -o -name "*.wav" | while read file; do
        if [ ! -r "$file" ]; then
            echo "Нет прав на чтение для файла: $file"
            chmod +r "$file" || echo "Не удалось установить права на чтение для $file"
        fi
    done
    
    # Запускаем приложение от имени appuser
    echo "Запуск приложения от пользователя appuser (UID=$OWNER_UID)"
    exec gosu appuser "$@"
else
    # Если владелец root, просто запускаем приложение
    echo "Запуск приложения от имени root"
    exec "$@"
fi 