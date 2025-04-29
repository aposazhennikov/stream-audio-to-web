#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Модуль для настройки логирования и интеграции с Sentry.
"""

import os
import logging
import traceback
import sentry_sdk
from sentry_sdk.integrations.logging import LoggingIntegration


def setup_logging():
    """
    Настраивает логирование и инициализирует Sentry для трекинга ошибок.
    
    Returns:
        logging.Logger: Настроенный логгер
    """
    try:
        # Создаем базовый логгер
        logger = logging.getLogger('audio_streamer')
        
        # Устанавливаем уровень детализации для вывода больше информации
        logger.setLevel(logging.DEBUG)
        
        # Обработчик для вывода логов в консоль
        console_handler = logging.StreamHandler()
        console_handler.setLevel(logging.INFO)
        
        # Формат сообщений лога
        formatter = logging.Formatter('%(asctime)s - %(name)s - %(levelname)s - %(message)s')
        console_handler.setFormatter(formatter)
        
        logger.addHandler(console_handler)
        
        # Настройка Sentry
        sentry_logging = LoggingIntegration(
            level=logging.DEBUG,     # Захватываем все логи для диагностики
            event_level=logging.ERROR  # Отправляем в Sentry только ошибки
        )
        
        # Получение информации о среде выполнения для Sentry
        environment = os.environ.get('ENVIRONMENT', 'development')
        release = os.environ.get('RELEASE', '1.0.0')
        
        # Инициализация Sentry с улучшенной конфигурацией
        logger.info("Инициализация Sentry...")
        sentry_sdk.init(
            dsn="https://6fc2e23a91bd96e4bda51d1f1c52b35a@o4508953992101888.ingest.de.sentry.io/4509237748629584",
            integrations=[sentry_logging],
            send_default_pii=True,
            environment=environment,
            release=release,
            traces_sample_rate=1.0,  # Трассировка для всех запросов
            # Включаем больше контекста для отлова проблем
            debug=True,
            max_breadcrumbs=50,      # Увеличиваем количество хлебных крошек
        )
        
        # Проверка подключения к Sentry
        try:
            # Тестовое событие для проверки подключения
            logger.info("Проверка подключения к Sentry...")
            sentry_sdk.capture_message("Приложение аудио-стриминга запущено", level="info")
            
            # Сразу же сбрасываем сообщение в Sentry для проверки подключения
            client = sentry_sdk.Hub.current.client
            if client:
                client.flush(timeout=2.0)
                logger.info("Тестовое сообщение отправлено в Sentry")
            else:
                logger.warning("Клиент Sentry не инициализирован")
        except Exception as e:
            logger.warning(f"Не удалось отправить тестовое сообщение в Sentry: {e}")
        
        logger.info("Логирование успешно настроено")
        return logger
    
    except Exception as e:
        # В случае ошибки выводим подробную информацию
        print(f"КРИТИЧЕСКАЯ ОШИБКА при настройке логирования: {e}")
        print(traceback.format_exc())
        
        # Настройка базового логгера в случае ошибки
        basic_logger = logging.getLogger('audio_streamer_basic')
        basic_logger.setLevel(logging.INFO)
        
        # Добавляем консольный обработчик
        basic_handler = logging.StreamHandler()
        basic_handler.setLevel(logging.INFO)
        basic_formatter = logging.Formatter('%(asctime)s - %(name)s - %(levelname)s - %(message)s')
        basic_handler.setFormatter(basic_formatter)
        basic_logger.addHandler(basic_handler)
        
        basic_logger.warning("Инициализирован базовый логгер из-за ошибки в настройке основного логгера")
        return basic_logger 