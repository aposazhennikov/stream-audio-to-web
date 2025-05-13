#!/bin/sh
set -e

# List of directories to check (removed /app/audio)
AUDIO_DIRS=""

# Adding additional directories from the DIRECTORY_ROUTES environment variable
if [ -n "$DIRECTORY_ROUTES" ]; then
    # Попытка извлечь директории из формата JSON
    DIRS_FROM_ENV=$(echo "$DIRECTORY_ROUTES" | grep -o '"/app/[^"]*"' | tr -d '"')
    
    # Проверяем, что удалось получить директории из JSON
    if [ -n "$DIRS_FROM_ENV" ]; then
        echo "Директории успешно извлечены из JSON формата DIRECTORY_ROUTES"
        for dir in $DIRS_FROM_ENV; do
            if [ -d "$dir" ]; then
                AUDIO_DIRS="$AUDIO_DIRS $dir"
            fi
        done
    else
        # Пробуем старый формат с разделителями
        echo "Директории не найдены в JSON формате, пробуем старый формат"
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

echo "Checking audio directories: $AUDIO_DIRS"

# Checking and fixing access permissions for directories and files
for dir in $AUDIO_DIRS; do
    if [ -d "$dir" ]; then
        echo "Checking directory: $dir"
        
        # If the directory is not readable for the current user
        if [ ! -r "$dir" ]; then
            echo "Insufficient permissions to read directory: $dir"
            
            # Attempt to apply permissions only if running as root
            if [ "$(id -u)" = "0" ]; then
                echo "Setting read permissions for directory: $dir"
                chmod -R +r "$dir" || echo "Failed to set read permissions for $dir"
            else
                echo "WARNING: Insufficient permissions to change access rights. Directory $dir may be unavailable."
            fi
        fi
        
        # Checking read permissions for all files in the directory
        find "$dir" -type f -not -readable 2>/dev/null | while read -r file; do
            echo "Found file without read permissions: $file"
            
            # Attempt to apply permissions only if running as root
            if [ "$(id -u)" = "0" ]; then
                echo "Setting read permissions for file: $file"
                chmod +r "$file" || echo "Failed to set read permissions for $file"
            else
                echo "WARNING: Insufficient permissions to change access rights. File $file may be unavailable."
            fi
        done
    else
        echo "Directory $dir does not exist, creating..."
        mkdir -p "$dir"
        chmod -R 755 "$dir"
    fi
done

# Configuring network stack for reliable HTTP server operation
echo "Configuring network stack for reliable HTTP server operation..."
sysctl -w net.ipv4.tcp_tw_reuse=1 || echo "WARNING: Failed to configure TCP TIME_WAIT reuse"

# Increasing file descriptor limit if possible
ulimit -n 4096 2>/dev/null || echo "Skipping descriptor limit increase (not supported)"

echo "Preparation completed, starting the application..."

# Running the main application
exec "$@" 