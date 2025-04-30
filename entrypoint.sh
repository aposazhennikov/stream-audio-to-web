#!/bin/bash
set -e

# Список директорий для проверки
AUDIO_DIRS=("/app/audio")

# Добавляем дополнительные директории из переменной окружения DIRECTORY_ROUTES
if [ -n "$DIRECTORY_ROUTES" ]; then
    # Извлекаем пути директорий из JSON строки
    # Примечание: это упрощенный парсинг, работает с основным форматом JSON
    DIRS_FROM_ENV=$(echo "$DIRECTORY_ROUTES" | grep -o '"/app/[^"]*"' | tr -d '"')
    for dir in $DIRS_FROM_ENV; do
        if [ -d "$dir" ]; then
            AUDIO_DIRS+=("$dir")
        fi
    done
fi

echo "Проверка следующих директорий с аудио: ${AUDIO_DIRS[*]}"

# Функция для проверки и настройки прав для одной директории
check_directory() {
    local dir=$1
    echo "Проверка владельца примонтированного каталога $dir"
    
    # Получаем ID пользователя и группы владельца примонтированного volume
    OWNER_UID=$(stat -c "%u" "$dir")
    OWNER_GID=$(stat -c "%g" "$dir")
    
    echo "Владелец каталога $dir: UID=$OWNER_UID, GID=$OWNER_GID"
    
    # Проверяем наличие прав на чтение для всех файлов в этой директории
    echo "Проверка прав доступа к файлам в $dir"
    find "$dir" -type f -name "*.mp3" -o -name "*.wav" | while read -r file; do
        if [ ! -r "$file" ]; then
            echo "Нет прав на чтение для файла: $file"
            chmod +r "$file" || echo "Не удалось установить права на чтение для $file"
        fi
    done
}

# Проверяем владельца и права для каждой директории
for dir in "${AUDIO_DIRS[@]}"; do
    if [ -d "$dir" ]; then
        check_directory "$dir"
    else
        echo "Директория $dir не существует, создаём..."
        mkdir -p "$dir"
        chmod 777 "$dir"
    fi
done

# Если владелец не root (UID 0), настраиваем пользователя appuser на эти UID/GID
# Используем UID первой директории как основной
PRIMARY_DIR="${AUDIO_DIRS[0]}"
PRIMARY_UID=$(stat -c "%u" "$PRIMARY_DIR")
PRIMARY_GID=$(stat -c "%g" "$PRIMARY_DIR")

if [ "$PRIMARY_UID" != "0" ]; then
    echo "Настройка пользователя appuser на UID=$PRIMARY_UID, GID=$PRIMARY_GID (на основе $PRIMARY_DIR)"
    groupmod -o -g "$PRIMARY_GID" appuser
    usermod -o -u "$PRIMARY_UID" appuser
    
    # Запускаем приложение от имени appuser
    echo "Запуск приложения от пользователя appuser (UID=$PRIMARY_UID)"
    exec gosu appuser "$@"
else
    # Если владелец root, просто запускаем приложение
    echo "Запуск приложения от имени root"
    exec "$@"
fi 