#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Модуль для работы с плейлистом аудиофайлов.
"""

import os
import logging
from typing import List, Optional


class PlaylistManager:
    """
    Класс для управления плейлистом аудиофайлов.
    
    Attributes:
        playlist_file (str): Путь к файлу плейлиста
        audio_files (List[str]): Список путей к аудиофайлам
        current_index (int): Индекс текущего воспроизводимого файла
    """
    
    def __init__(self, playlist_file: str):
        """
        Инициализирует менеджер плейлиста.
        
        Args:
            playlist_file (str): Путь к файлу плейлиста
        """
        self.logger = logging.getLogger('audio_streamer')
        self.playlist_file = playlist_file
        self.audio_files = []
        self.current_index = 0
        
        self._load_playlist()
    
    def _load_playlist(self) -> None:
        """
        Загружает список аудиофайлов из файла плейлиста.
        Проверяет существование файлов и их формат (.mp3 или .wav).
        """
        try:
            if not os.path.exists(self.playlist_file):
                self.logger.error(f"Файл плейлиста {self.playlist_file} не найден")
                return
            
            with open(self.playlist_file, 'r') as f:
                lines = f.readlines()
            
            self.audio_files = []
            for line in lines:
                file_path = line.strip()
                if not file_path:
                    continue
                
                if not os.path.exists(file_path):
                    self.logger.warning(f"Аудиофайл не найден: {file_path}")
                    continue
                
                extension = os.path.splitext(file_path)[1].lower()
                if extension not in ['.mp3', '.wav']:
                    self.logger.warning(f"Неподдерживаемый формат файла: {file_path}")
                    continue
                
                self.audio_files.append(file_path)
            
            if not self.audio_files:
                self.logger.warning("Плейлист пуст или не содержит доступных аудиофайлов")
            else:
                self.logger.info(f"Загружено {len(self.audio_files)} аудиофайлов")
        
        except Exception as e:
            self.logger.error(f"Ошибка при загрузке плейлиста: {e}", exc_info=True)
    
    def get_next_track(self) -> Optional[str]:
        """
        Возвращает путь к следующему аудиофайлу в плейлисте.
        
        Returns:
            Optional[str]: Путь к следующему аудиофайлу или None, если плейлист пуст
        """
        try:
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
        Перезагружает плейлист из файла.
        """
        try:
            self.current_index = 0
            self._load_playlist()
        except Exception as e:
            self.logger.error(f"Ошибка при перезагрузке плейлиста: {e}", exc_info=True) 