#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Модуль для обработки аудиофайлов и создания аудиопотока.
"""

import os
import logging
import tempfile
import shutil
import stat
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
    
    def check_file_access(self, file_path: str) -> bool:
        """
        Проверяет доступность файла для чтения.
        
        Args:
            file_path (str): Путь к файлу
            
        Returns:
            bool: True если файл существует и доступен для чтения
        """
        try:
            if not os.path.exists(file_path):
                self.logger.error(f"Файл не существует: {file_path}")
                return False
            
            if not os.access(file_path, os.R_OK):
                file_stat = os.stat(file_path)
                self.logger.error(f"Нет прав на чтение файла: {file_path}")
                self.logger.debug(f"Права файла: {oct(file_stat.st_mode)}")
                return False
            
            return True
        except Exception as e:
            self.logger.error(f"Ошибка при проверке доступа к файлу {file_path}: {e}")
            return False
    
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
            # Получаем имя файла для логирования
            file_name = os.path.basename(audio_file_path)
            self.logger.info(f"Загрузка аудиофайла: {file_name}")
            
            # Проверка доступа к файлу
            if not self.check_file_access(audio_file_path):
                raise PermissionError(f"Нет доступа к файлу: {audio_file_path}")
            
            audio_format = self.get_audio_format(audio_file_path)
            self.logger.info(f"Определен формат файла: {audio_format}")
            
            # Пробуем загрузить файл разными способами
            try:
                # Стандартный метод загрузки
                self.logger.debug(f"Попытка загрузки стандартным методом")
                audio = AudioSegment.from_file(audio_file_path, format=audio_format)
            except Exception as e:
                self.logger.warning(f"Стандартный метод загрузки не сработал: {e}")
                
                # Создаем временную копию файла с безопасным именем
                temp_dir = tempfile.mkdtemp()
                temp_file_path = os.path.join(temp_dir, f"temp_audio.{audio_format}")
                
                try:
                    self.logger.info(f"Копирование во временный файл: {temp_file_path}")
                    shutil.copy2(audio_file_path, temp_file_path)
                    
                    # Устанавливаем правильные права на временный файл
                    os.chmod(temp_file_path, 0o644)  # rw-r--r--
                    
                    self.logger.info(f"Загрузка из временного файла")
                    audio = AudioSegment.from_file(temp_file_path, format=audio_format)
                    
                    # Удаляем временную директорию
                    shutil.rmtree(temp_dir, ignore_errors=True)
                except Exception as e2:
                    self.logger.error(f"Вторая попытка загрузки не удалась: {e2}", exc_info=True)
                    shutil.rmtree(temp_dir, ignore_errors=True)
                    raise
            
            self.logger.info(f"Аудиофайл успешно загружен, создание MP3 потока")
            
            # Создаем временный буфер для MP3
            buffer = io.BytesIO()
            audio.export(buffer, format="mp3", bitrate="192k")
            
            # Подготавливаем буфер для чтения
            buffer.seek(0)
            size = buffer.getbuffer().nbytes
            
            self.logger.info(f"MP3 поток создан, размер: {size} байт")
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
            # Логируем информацию о треке
            file_name = os.path.basename(file_path)
            self.logger.info(f"Начало обработки трека: {file_name}")
            
            if not self.check_file_access(file_path):
                self.logger.error(f"Файл недоступен для чтения: {file_path}")
                return None
            
            return self.convert_to_mp3_stream(file_path)
        
        except Exception as e:
            self.logger.error(f"Ошибка при создании потока из файла {file_path}: {e}", exc_info=True)
            return None 