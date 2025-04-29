#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Главный модуль приложения для аудио стриминга.
Инициализирует и запускает веб-сервер для потоковой передачи аудио.
"""

import logging
from logger import setup_logging
from playlist_manager import PlaylistManager
from web_server import AudioStreamServer


def main():
    """
    Главная функция приложения.
    Инициализирует логгер, менеджер плейлиста и запускает веб-сервер.
    """
    try:
        # Настройка логирования
        logger = setup_logging()
        logger.info("Запуск аудио стриминг-сервера")

        # Инициализация менеджера плейлиста
        playlist_manager = PlaylistManager("playlist.txt")
        
        # Запуск аудио-сервера
        server = AudioStreamServer(playlist_manager)
        server.run(host="0.0.0.0", port=9999)
        
    except Exception as e:
        logging.error(f"Ошибка при запуске приложения: {e}", exc_info=True)


if __name__ == "__main__":
    main() 