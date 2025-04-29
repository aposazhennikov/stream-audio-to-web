#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Модуль для обработки аудиофайлов и создания аудиопотока.
"""

import os
import logging
import tempfile
from typing import Optional, BinaryIO, Tuple
from pydub import AudioSegment
import io


class AudioStreamer:
    """
    Класс для обработки аудиофайлов и создания потокового аудио.
    """
    
    def __init__(self):
        """
        Инициализирует объект обработчика аудиопотока.
        """
        self.logger = logging.getLogger('audio_streamer')
        self.chunk_size = 4096  # Размер чанка данных для потоковой передачи
    
    def get_audio_format(self, file_path: str) -> str:
        """
        Определяет формат аудиофайла по расширению.
        
        Args:
            file_path (str): Путь к аудиофайлу
            
        Returns:
            str: Формат аудио ('mp3' или 'wav')
        """
        try:
            _, ext = os.path.splitext(file_path)
            return ext.lower().lstrip('.')
        except Exception as e:
            self.logger.error(f"Ошибка при определении формата аудио: {e}", exc_info=True)
            return 'mp3'  # Возвращаем mp3 по умолчанию
    
    def convert_to_mp3_stream(self, audio_file_path: str) -> Tuple[BinaryIO, int]:
        """
        Конвертирует аудиофайл в MP3-поток для веб-трансляции.
        
        Args:
            audio_file_path (str): Путь к аудиофайлу
            
        Returns:
            Tuple[BinaryIO, int]: Поток данных и его размер
        """
        try:
            # Загружаем аудиофайл с помощью pydub
            self.logger.info(f"Загрузка аудиофайла: {audio_file_path}")
            audio_format = self.get_audio_format(audio_file_path)
            
            audio = AudioSegment.from_file(audio_file_path, format=audio_format)
            
            # Создаем временный буфер для MP3
            buffer = io.BytesIO()
            audio.export(buffer, format="mp3", bitrate="192k")
            
            # Подготавливаем буфер для чтения
            buffer.seek(0)
            size = buffer.getbuffer().nbytes
            
            return buffer, size
        
        except Exception as e:
            self.logger.error(f"Ошибка при конвертации аудиофайла {audio_file_path}: {e}", exc_info=True)
            raise
    
    def create_stream_from_file(self, file_path: str) -> Optional[Tuple[BinaryIO, int]]:
        """
        Создает аудиопоток из файла для веб-трансляции.
        
        Args:
            file_path (str): Путь к аудиофайлу
            
        Returns:
            Optional[Tuple[BinaryIO, int]]: Кортеж из потока данных и его размера или None при ошибке
        """
        try:
            if not os.path.exists(file_path):
                self.logger.error(f"Аудиофайл не найден: {file_path}")
                return None
            
            return self.convert_to_mp3_stream(file_path)
        
        except Exception as e:
            self.logger.error(f"Ошибка при создании потока из файла {file_path}: {e}", exc_info=True)
            return None 