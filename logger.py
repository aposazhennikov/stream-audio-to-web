#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Модуль для настройки логирования и интеграции с Sentry.
"""

import logging
import sentry_sdk
from sentry_sdk.integrations.logging import LoggingIntegration


def setup_logging():
    """
    Настраивает логирование и инициализирует Sentry для трекинга ошибок.
    
    Returns:
        logging.Logger: Настроенный логгер
    """
    try:
        # Настройка Sentry
        sentry_logging = LoggingIntegration(
            level=logging.INFO,
            event_level=logging.ERROR
        )
        
        sentry_sdk.init(
            dsn="https://6fc2e23a91bd96e4bda51d1f1c52b35a@o4508953992101888.ingest.de.sentry.io/4509237748629584",
            send_default_pii=True,
            integrations=[sentry_logging],
        )
        
        # Настройка логирования
        logger = logging.getLogger('audio_streamer')
        logger.setLevel(logging.INFO)
        
        # Обработчик для вывода логов в консоль
        console_handler = logging.StreamHandler()
        console_handler.setLevel(logging.INFO)
        
        # Формат сообщений лога
        formatter = logging.Formatter('%(asctime)s - %(name)s - %(levelname)s - %(message)s')
        console_handler.setFormatter(formatter)
        
        logger.addHandler(console_handler)
        
        return logger
    
    except Exception as e:
        print(f"Ошибка при настройке логирования: {e}")
        # Настройка базового логгера в случае ошибки
        basic_logger = logging.getLogger('audio_streamer_basic')
        basic_logger.setLevel(logging.INFO)
        return basic_logger 