#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Главный модуль приложения для аудио стриминга.
Инициализирует и запускает веб-сервер для потоковой передачи аудио.
"""

import os
import logging
import argparse
import json
from typing import Dict
from logger import setup_logging
from playlist_manager import PlaylistManager
from web_server import AudioStreamServer


def parse_arguments():
    """
    Парсит аргументы командной строки.
    
    Returns:
        argparse.Namespace: Объект с аргументами командной строки
    """
    parser = argparse.ArgumentParser(description='Аудио стриминг сервер')
    parser.add_argument('--audio-dir', type=str, default='/app/audio',
                        help='Путь к директории с аудиофайлами (по умолчанию: /app/audio)')
    parser.add_argument('--port', type=int, default=9999,
                        help='Порт для веб-сервера (по умолчанию: 9999)')
    parser.add_argument('--host', type=str, default='0.0.0.0',
                        help='Хост для веб-сервера (по умолчанию: 0.0.0.0)')
    parser.add_argument('--stream-routes', type=str, default=None,
                        help='Список URL-путей для аудиопотока, разделённых запятыми (например: "/,/humor,/science")')
    parser.add_argument('--directory-routes', type=str, default=None,
                        help='JSON строка с сопоставлением директорий и маршрутов. ' +
                             'Пример: \'{"humor":"/app/humor","science":"/app/science"}\'')
    
    return parser.parse_args()


def parse_directory_routes(dir_routes_str: str) -> Dict[str, str]:
    """
    Парсит строку JSON с сопоставлением директорий и маршрутов.
    
    Args:
        dir_routes_str (str): JSON строка с сопоставлением
        
    Returns:
        Dict[str, str]: Словарь маршрутов и соответствующих им директорий
    """
    if not dir_routes_str:
        return {}
    
    try:
        routes_dict = json.loads(dir_routes_str)
        
        # Приводим к формату {route: directory}
        result = {}
        for route_key, directory in routes_dict.items():
            # Убеждаемся, что маршрут начинается с '/'
            route = route_key if route_key.startswith('/') else f'/{route_key}'
            result[route] = directory
        
        return result
    except json.JSONDecodeError as e:
        logging.error(f"Ошибка при парсинге JSON с маршрутами: {e}")
        return {}
    except Exception as e:
        logging.error(f"Неизвестная ошибка при обработке маршрутов: {e}")
        return {}


def main():
    """
    Главная функция приложения.
    Инициализирует логгер, менеджер плейлиста и запускает веб-сервер.
    """
    try:
        # Парсинг аргументов командной строки
        args = parse_arguments()
        
        # Настройка логирования
        logger = setup_logging()
        logger.info("Запуск аудио стриминг-сервера")
        
        # Проверка существования директории с аудио по умолчанию
        if not os.path.exists(args.audio_dir):
            logger.warning(f"Директория с аудиофайлами по умолчанию не найдена: {args.audio_dir}")
            logger.info(f"Создание директории: {args.audio_dir}")
            os.makedirs(args.audio_dir, exist_ok=True)
        
        # Если указаны маршруты как аргумент командной строки, 
        # устанавливаем их в переменную окружения
        if args.stream_routes:
            logger.info(f"Установка маршрутов из аргументов командной строки: {args.stream_routes}")
            os.environ['AUDIO_STREAM_ROUTES'] = args.stream_routes
        
        # Обработка сопоставления директорий и маршрутов
        directory_routes = {}
        
        # Приоритет 1: Аргумент командной строки --directory-routes
        if args.directory_routes:
            logger.info(f"Обработка сопоставлений директорий и маршрутов из аргументов: {args.directory_routes}")
            directory_routes = parse_directory_routes(args.directory_routes)
        
        # Приоритет 2: Переменная окружения DIRECTORY_ROUTES
        elif 'DIRECTORY_ROUTES' in os.environ:
            env_routes = os.environ.get('DIRECTORY_ROUTES')
            logger.info(f"Обработка сопоставлений директорий и маршрутов из переменной окружения: {env_routes}")
            directory_routes = parse_directory_routes(env_routes)
        
        # Вывод информации о сопоставлении маршрутов и директорий
        if directory_routes:
            logger.info("Настроены следующие сопоставления маршрутов и директорий:")
            for route, directory in directory_routes.items():
                logger.info(f"  Маршрут: {route} -> Директория: {directory}")
                
                # Проверяем существование директории и создаем при необходимости
                if not os.path.exists(directory):
                    logger.warning(f"Директория для маршрута {route} не найдена: {directory}")
                    logger.info(f"Создание директории: {directory}")
                    os.makedirs(directory, exist_ok=True)
        
        # Инициализация менеджера плейлиста с поддержкой разных директорий для разных маршрутов
        playlist_manager = PlaylistManager(args.audio_dir, directory_routes)
        
        # Запуск аудио-сервера
        server = AudioStreamServer(playlist_manager)
        server.run(host=args.host, port=args.port)
        
    except Exception as e:
        logging.error(f"Ошибка при запуске приложения: {e}", exc_info=True)


if __name__ == "__main__":
    main() 