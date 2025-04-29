#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Главный модуль приложения для аудио стриминга.
Инициализирует и запускает веб-сервер для потоковой передачи аудио.
"""

import os
import logging
import argparse
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
        
        # Проверка существования директории с аудио
        if not os.path.exists(args.audio_dir):
            logger.warning(f"Директория с аудиофайлами не найдена: {args.audio_dir}")
            logger.info(f"Создание директории: {args.audio_dir}")
            os.makedirs(args.audio_dir, exist_ok=True)
        
        # Инициализация менеджера плейлиста
        playlist_manager = PlaylistManager(args.audio_dir)
        
        # Запуск аудио-сервера
        server = AudioStreamServer(playlist_manager)
        server.run(host=args.host, port=args.port)
        
    except Exception as e:
        logging.error(f"Ошибка при запуске приложения: {e}", exc_info=True)


if __name__ == "__main__":
    main() 