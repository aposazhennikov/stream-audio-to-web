#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Модуль для работы с плейлистом аудиофайлов.
"""

import os
import random
import logging
from typing import List, Optional


class PlaylistManager:
    """
    Класс для управления плейлистом аудиофайлов.
    
    Attributes:
        audio_directory (str): Путь к директории с аудиофайлами
        audio_files (List[str]): Список путей к аудиофайлам
        current_index (int): Индекс текущего воспроизводимого файла
    """
    
    def __init__(self, audio_directory: str):
        """
        Инициализирует менеджер плейлиста.
        
        Args:
            audio_directory (str): Путь к директории с аудиофайлами
        """
        self.logger = logging.getLogger('audio_streamer')
        self.audio_directory = audio_directory
        self.audio_files = []
        self.current_index = 0
        
        self._scan_directory()
    
    def _scan_directory(self) -> None:
        """
        Сканирует директорию и находит все поддерживаемые аудиофайлы (.mp3 или .wav).
        """
        try:
            if not os.path.exists(self.audio_directory):
                self.logger.error(f"Директория {self.audio_directory} не найдена")
                return
            
            self.audio_files = []
            
            # Рекурсивно обходим все файлы в директории
            for root, _, files in os.walk(self.audio_directory, followlinks=True):
                for filename in files:
                    try:
                        # Проверяем расширение файла
                        extension = os.path.splitext(filename)[1].lower()
                        if extension not in ['.mp3', '.wav']:
                            continue
                        
                        # Полный путь к файлу
                        file_path = os.path.join(root, filename)
                        
                        # Проверяем существование файла
                        if not os.path.exists(file_path):
                            continue
                        
                        self.audio_files.append(file_path)
                    except Exception as e:
                        self.logger.warning(f"Ошибка при обработке файла {filename}: {e}")
            
            # Перемешиваем файлы для разнообразия воспроизведения
            random.shuffle(self.audio_files)
            
            if not self.audio_files:
                self.logger.warning(f"В директории {self.audio_directory} не найдено аудиофайлов (.mp3, .wav)")
            else:
                self.logger.info(f"Найдено {len(self.audio_files)} аудиофайлов в {self.audio_directory}")
        
        except Exception as e:
            self.logger.error(f"Ошибка при сканировании директории: {e}", exc_info=True)
    
    def get_next_track(self) -> Optional[str]:
        """
        Возвращает путь к следующему аудиофайлу в плейлисте.
        
        Returns:
            Optional[str]: Путь к следующему аудиофайлу или None, если плейлист пуст
        """
        try:
            if not self.audio_files:
                # Повторно сканируем директорию, возможно, появились новые файлы
                self._scan_directory()
                if not self.audio_files:
                    return None
            
            track = self.audio_files[self.current_index]
            self.current_index = (self.current_index + 1) % len(self.audio_files)
            return track
        
        except Exception as e:
            self.logger.error(f"Ошибка при получении следующего трека: {e}", exc_info=True)
            return None
    
    def get_current_playlist(self) -> List[str]:
        """
        Возвращает текущий список аудиофайлов.
        
        Returns:
            List[str]: Список путей к аудиофайлам
        """
        return self.audio_files.copy()
    
    def reload_playlist(self) -> None:
        """
        Перезагружает плейлист, сканируя директорию заново.
        """
        try:
            self.current_index = 0
            self._scan_directory()
        except Exception as e:
            self.logger.error(f"Ошибка при перезагрузке плейлиста: {e}", exc_info=True) 