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
from web_server import AudioStreamServer, parse_folder_route_map


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
    parser.add_argument('--folder-route-map', type=str, default=None,
                        help='Карта соответствия папок и маршрутов в формате "путь1:маршрут1,путь2:маршрут2"')
    
    return parser.parse_args()


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
        
        # Обработка аргументов командной строки и переменных окружения
        folder_route_map = None
        
        # Если указана карта папок и маршрутов как аргумент командной строки,
        # устанавливаем ее в переменную окружения
        if args.folder_route_map:
            logger.info(f"Установка карты папок и маршрутов из аргументов командной строки: {args.folder_route_map}")
            os.environ['FOLDER_ROUTE_MAP'] = args.folder_route_map
        else:
            # Если карта не указана, проверяем наличие standard_audio_dir
            # и создаем простую карту из одной папки
            if not os.environ.get('FOLDER_ROUTE_MAP'):
                audio_dir = args.audio_dir
                
                # Проверка существования директории с аудио
                if not os.path.exists(audio_dir):
                    logger.warning(f"Директория с аудиофайлами не найдена: {audio_dir}")
                    logger.info(f"Создание директории: {audio_dir}")
                    os.makedirs(audio_dir, exist_ok=True)
                
                # Устанавливаем переменную окружения AUDIO_DIR для обратной совместимости
                os.environ['AUDIO_DIR'] = audio_dir
                logger.info(f"Установлена переменная окружения AUDIO_DIR: {audio_dir}")
                
                # Если указаны маршруты как аргумент командной строки, 
                # устанавливаем их в переменную окружения
                if args.stream_routes:
                    logger.info(f"Установка маршрутов из аргументов командной строки: {args.stream_routes}")
                    os.environ['AUDIO_STREAM_ROUTES'] = args.stream_routes
        
        # Получаем карту папок и маршрутов из переменной окружения
        folder_route_map_str = os.environ.get('FOLDER_ROUTE_MAP')
        if folder_route_map_str:
            folder_route_map = parse_folder_route_map(folder_route_map_str)
            logger.info(f"Карта папок и маршрутов из переменной окружения: {folder_route_map}")
        
        # Запуск аудио-сервера с картой папок и маршрутов
        server = AudioStreamServer(folder_route_map)
        server.run(host=args.host, port=args.port)
        
    except Exception as e:
        logging.error(f"Ошибка при запуске приложения: {e}", exc_info=True)


if __name__ == "__main__":
    main() 