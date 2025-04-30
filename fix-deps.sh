#!/bin/sh
set -e

echo "Очистка кэша зависимостей..."
rm -f go.sum

echo "Проверка версии Go..."
go version

echo "Очистка кэша модулей..."
go clean -modcache

echo "Загрузка зависимостей заново..."
go mod tidy
go mod verify

echo "Выполнение тестовой загрузки зависимостей..."
go mod download

echo "Проверка зависимостей успешно завершена!" 