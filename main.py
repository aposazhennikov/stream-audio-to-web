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
import sys


def parse_args():
    """
    Разбирает аргументы командной строки.
    
    Returns:
        argparse.Namespace: Объект с аргументами командной строки
    """
    parser = argparse.ArgumentParser(description='Аудио стриминг сервер')
    parser.add_argument('--audio-dir', type=str, default='/app/audio',
                       help='Путь к директории с аудиофайлами')
    parser.add_argument('--port', type=int, default=9999,
                       help='Порт для веб-сервера')
    parser.add_argument('--host', type=str, default='0.0.0.0',
                       help='Хост для веб-сервера')
    parser.add_argument('--stream-routes', type=str, default=None,
                       help='Список URL-путей для аудиопотока, разделенных запятыми (например: "/,/humor,/science")')
    parser.add_argument('--directory-routes', type=str, default=None,
                       help='JSON строка с сопоставлением маршрутов и директорий (например: {"humor":"/app/humor","science":"/app/science"})')
    parser.add_argument('--stream-mode', type=str, default='radio', choices=['radio', 'individual'],
                       help='Режим стриминга: "radio" (все слушатели слышат один поток) или "individual" (у каждого слушателя свой поток)')
    parser.add_argument('--buffer-size', type=int, default=2 * 1024 * 1024,
                       help='Размер буфера аудиопотока в байтах (по умолчанию: 2MB)')
    parser.add_argument('--max-preload-tracks', type=int, default=2,
                       help='Максимальное количество предзагружаемых треков (по умолчанию: 2)')
    
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
    Основная функция приложения.
    """
    # Получение аргументов командной строки
    args = parse_args()
    
    # Инициализация логгера
    logger = setup_logging()
    
    try:
        # Получаем пути к аудиофайлам из аргументов командной строки
        audio_directory = args.audio_dir
        
        # Получаем маршруты из переменных окружения или аргументов командной строки
        stream_routes = None
        if args.stream_routes:
            stream_routes = args.stream_routes.split(',')
        elif os.environ.get('AUDIO_STREAM_ROUTES'):
            stream_routes = os.environ.get('AUDIO_STREAM_ROUTES').split(',')
        
        # Получаем сопоставление директорий и маршрутов
        directory_routes = None
        if args.directory_routes:
            try:
                directory_routes = json.loads(args.directory_routes)
            except json.JSONDecodeError as e:
                logger.error(f"Ошибка при разборе JSON строки directory_routes: {e}")
        elif os.environ.get('DIRECTORY_ROUTES'):
            try:
                directory_routes = json.loads(os.environ.get('DIRECTORY_ROUTES'))
            except json.JSONDecodeError as e:
                logger.error(f"Ошибка при разборе JSON строки из переменной DIRECTORY_ROUTES: {e}")
        
        # Получаем режим стриминга
        stream_mode = args.stream_mode
        if os.environ.get('STREAM_MODE'):
            stream_mode = os.environ.get('STREAM_MODE')
            
        # Получаем размер буфера из переменной окружения или аргумента командной строки
        buffer_size = args.buffer_size
        if os.environ.get('BUFFER_SIZE'):
            try:
                buffer_size = int(os.environ.get('BUFFER_SIZE'))
            except ValueError:
                logger.warning(f"Некорректное значение BUFFER_SIZE: {os.environ.get('BUFFER_SIZE')}, используем значение по умолчанию")
        
        # Получаем максимальное количество предзагружаемых треков
        max_preload_tracks = args.max_preload_tracks
        if os.environ.get('MAX_PRELOAD_TRACKS'):
            try:
                max_preload_tracks = int(os.environ.get('MAX_PRELOAD_TRACKS'))
            except ValueError:
                logger.warning(f"Некорректное значение MAX_PRELOAD_TRACKS: {os.environ.get('MAX_PRELOAD_TRACKS')}, используем значение по умолчанию")
        
        # Вывод информации о конфигурации
        logger.info(f"Аудио директория: {audio_directory}")
        logger.info(f"Режим стриминга: {stream_mode}")
        logger.info(f"Размер буфера: {buffer_size / 1024 / 1024:.2f} MB")
        logger.info(f"Максимальное количество предзагружаемых треков: {max_preload_tracks}")
        
        if stream_routes:
            logger.info(f"Маршруты аудиопотоков: {stream_routes}")
        
        if directory_routes:
            logger.info(f"Сопоставление директорий и маршрутов: {directory_routes}")
        
        # Инициализация PlaylistManager
        playlist_manager = PlaylistManager(
            default_audio_directory=audio_directory,
            directory_routes=directory_routes,
            stream_mode=stream_mode,
            buffer_size=buffer_size,
            max_preload_tracks=max_preload_tracks
        )
        
        # Создание и запуск веб-сервера
        server = AudioStreamServer(playlist_manager)
        server.run(host=args.host, port=args.port)
    
    except Exception as e:
        logger.error(f"Критическая ошибка в основном потоке: {e}", exc_info=True)
        sys.exit(1)


if __name__ == "__main__":
    main() 