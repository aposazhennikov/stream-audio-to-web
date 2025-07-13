#!/bin/sh
set -e

# Check log level to control script output
should_log_info() {
    case "${LOG_LEVEL:-warning}" in
        "debug"|"info") return 0 ;;
        *) return 1 ;;
    esac
}

should_log_warning() {
    case "${LOG_LEVEL:-warning}" in
        "debug"|"info"|"warning") return 0 ;;
        *) return 1 ;;
    esac
}

# List of directories to check (removed /app/audio)
AUDIO_DIRS=""

# Adding additional directories from the DIRECTORY_ROUTES environment variable
if [ -n "$DIRECTORY_ROUTES" ]; then
    # Попытка извлечь директории из формата JSON
    DIRS_FROM_ENV=$(echo "$DIRECTORY_ROUTES" | grep -o '"/app/[^"]*"' | tr -d '"')
    
    # Проверяем, что удалось получить директории из JSON
    if [ -n "$DIRS_FROM_ENV" ]; then
        should_log_info && echo "Директории успешно извлечены из JSON формата DIRECTORY_ROUTES"
        for dir in $DIRS_FROM_ENV; do
            if [ -d "$dir" ]; then
                AUDIO_DIRS="$AUDIO_DIRS $dir"
            fi
        done
    else
        # Пробуем старый формат с разделителями
        should_log_info && echo "Директории не найдены в JSON формате, пробуем старый формат"
        DIRS_OLD=$(echo "$DIRECTORY_ROUTES" | grep -o '/app/[^:]*' || echo "")
        for dir in $DIRS_OLD; do
            if [ -d "$dir" ]; then
                AUDIO_DIRS="$AUDIO_DIRS $dir"
            fi
        done
    fi
fi

# If the variable is empty, set default values for humor and science
if [ -z "$AUDIO_DIRS" ]; then
    AUDIO_DIRS="/app/humor /app/science"
fi

should_log_info && echo "Checking audio directories: $AUDIO_DIRS"

# Выводим информацию о текущем пользователе
should_log_info && echo "Current user: $(id -un) ($(id -u)), groups: $(id -gn) ($(id -g))"

# Checking and fixing access permissions for directories and files
for dir in $AUDIO_DIRS; do
    if [ -d "$dir" ]; then
        should_log_info && echo "Checking directory: $dir"
        should_log_info && echo "Directory ownership and permissions: $(ls -ld "$dir")"
        
        # If the directory is not readable for the current user
        if [ ! -r "$dir" ]; then
            should_log_warning && echo "Insufficient permissions to read directory: $dir"
            
            # Attempt to apply permissions only if running as root
            if [ "$(id -u)" = "0" ]; then
                should_log_info && echo "Setting read permissions for directory: $dir"
                chmod -R +r "$dir" || (should_log_warning && echo "Failed to set read permissions for $dir")
            else
                should_log_warning && echo "WARNING: Insufficient permissions to change access rights. Directory $dir may be unavailable."
            fi
        fi
        
        # Выводим список всех файлов в директории с правами доступа
        if should_log_info; then
            echo "Files in $dir:"
            ls -la "$dir"
        fi
        
        # Проверяем количество аудиофайлов по расширениям
        if should_log_info; then
            echo "Audio files in $dir:"
            MP3_COUNT=$(find "$dir" -type f -name "*.mp3" | wc -l)
            OGG_COUNT=$(find "$dir" -type f -name "*.ogg" | wc -l)
            AAC_COUNT=$(find "$dir" -type f -name "*.aac" | wc -l)
            echo "MP3 files: $MP3_COUNT, OGG files: $OGG_COUNT, AAC files: $AAC_COUNT"
        fi
        
        # Checking read permissions for all files in the directory
        find "$dir" -type f -not -readable 2>/dev/null | while read -r file; do
            should_log_info && echo "Found file without read permissions: $file"
            
            # Attempt to apply permissions only if running as root
            if [ "$(id -u)" = "0" ]; then
                should_log_info && echo "Setting read permissions for file: $file"
                chmod +r "$file" || (should_log_warning && echo "Failed to set read permissions for $file")
            else
                should_log_warning && echo "WARNING: Insufficient permissions to change access rights. File $file may be unavailable."
            fi
        done
    else
        should_log_info && echo "Directory $dir does not exist, creating..."
        mkdir -p "$dir"
        chmod -R 755 "$dir"
    fi
done

# Configuring network stack for reliable HTTP server operation
should_log_info && echo "Configuring network stack for reliable HTTP server operation..."
sysctl -w net.ipv4.tcp_tw_reuse=1 || (should_log_warning && echo "WARNING: Failed to configure TCP TIME_WAIT reuse")

# Increasing file descriptor limit if possible
ulimit -n 4096 2>/dev/null || (should_log_info && echo "Skipping descriptor limit increase (not supported)")

should_log_info && echo "Preparation completed, starting the application..."

# Running the main application
exec "$@" 