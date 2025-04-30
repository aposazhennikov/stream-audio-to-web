#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Модуль для работы с плейлистом аудиофайлов.
"""

import os
import random
import logging
import stat
from typing import List, Optional, Dict


class DirectoryPlaylist:
    """
    Класс для управления плейлистом из одной директории.
    
    Attributes:
        audio_directory (str): Путь к директории с аудиофайлами
        audio_files (List[str]): Список путей к аудиофайлам
        current_index (int): Индекс текущего воспроизводимого файла
    """
    
    def __init__(self, audio_directory: str, logger):
        """
        Инициализирует плейлист для одной директории.
        
        Args:
            audio_directory (str): Путь к директории с аудиофайлами
            logger: Логгер для записи сообщений
        """
        self.logger = logger
        self.audio_directory = audio_directory
        self.audio_files = []
        self.current_index = 0
        
        self._scan_directory()
    
    def _check_file_permissions(self, file_path: str) -> bool:
        """
        Проверяет права доступа к файлу.
        
        Args:
            file_path (str): Путь к файлу
            
        Returns:
            bool: True если файл доступен для чтения, иначе False
        """
        try:
            # Получаем информацию о файле
            file_stat = os.stat(file_path)
            
            # Проверяем права доступа
            readable = bool(file_stat.st_mode & stat.S_IRUSR)  # Проверка прав на чтение для владельца
            
            # Логируем отсутствие прав доступа
            if not readable:
                self.logger.warning(f"Нет прав на чтение файла: {file_path}")
                
                # Дополнительно выводим информацию о правах
                mode = file_stat.st_mode
                self.logger.debug(f"Права файла: {oct(mode)}, UID: {file_stat.st_uid}, GID: {file_stat.st_gid}")
                
                # Пробуем вывести текущего пользователя
                try:
                    import pwd
                    current_user = pwd.getpwuid(os.getuid()).pw_name
                    self.logger.debug(f"Текущий пользователь: {current_user} (UID: {os.getuid()})")
                except:
                    self.logger.debug(f"Не удалось определить текущего пользователя")
            
            return readable
        except Exception as e:
            self.logger.error(f"Ошибка при проверке прав доступа к файлу {file_path}: {e}")
            return False
    
    def _scan_directory(self) -> None:
        """
        Сканирует директорию и находит все поддерживаемые аудиофайлы (.mp3 или .wav).
        """
        try:
            if not os.path.exists(self.audio_directory):
                self.logger.error(f"Директория {self.audio_directory} не найдена")
                return
            
            self.logger.info(f"Сканирование директории: {self.audio_directory}")
            self.audio_files = []
            
            # Проверяем права доступа к директории
            if not os.access(self.audio_directory, os.R_OK):
                self.logger.error(f"Нет прав на чтение директории: {self.audio_directory}")
                
                try:
                    # Выводим информацию о правах директории
                    dir_stat = os.stat(self.audio_directory)
                    self.logger.debug(f"Права директории: {oct(dir_stat.st_mode)}")
                    self.logger.debug(f"UID директории: {dir_stat.st_uid}, GID директории: {dir_stat.st_gid}")
                    
                    # Выводим информацию о текущем пользователе
                    import pwd
                    current_user = pwd.getpwuid(os.getuid()).pw_name
                    self.logger.debug(f"Текущий пользователь: {current_user} (UID: {os.getuid()})")
                except Exception as e:
                    self.logger.debug(f"Ошибка при получении информации о правах: {e}")
                
                return
            
            # Прямой поиск mp3 и wav файлов без рекурсии
            for f in os.listdir(self.audio_directory):
                try:
                    file_path = os.path.join(self.audio_directory, f)
                    if os.path.isfile(file_path):
                        extension = os.path.splitext(f)[1].lower()
                        if extension in ['.mp3', '.wav']:
                            # Проверяем права доступа к файлу
                            if self._check_file_permissions(file_path):
                                self.audio_files.append(file_path)
                                self.logger.debug(f"Добавлен файл: {f}")
                            else:
                                self.logger.warning(f"Файл пропущен из-за отсутствия прав: {f}")
                except Exception as e:
                    self.logger.warning(f"Ошибка при обработке файла {f}: {e}")
            
            # Перемешиваем файлы для разнообразия воспроизведения
            random.shuffle(self.audio_files)
            
            if not self.audio_files:
                self.logger.warning(f"В директории {self.audio_directory} не найдено доступных аудиофайлов (.mp3, .wav)")
            else:
                self.logger.info(f"Найдено {len(self.audio_files)} аудиофайлов в {self.audio_directory}")
                # Выводим первые 5 файлов для отладки
                for i, file_path in enumerate(self.audio_files[:5]):
                    file_name = os.path.basename(file_path)
                    self.logger.info(f"Пример файла {i+1}: {file_name}")
        
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
            self.logger.info(f"Воспроизведение трека: {os.path.basename(track)}")
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


class PlaylistManager:
    """
    Класс для управления несколькими плейлистами, отображенных на разные маршруты.
    
    Attributes:
        playlists (Dict[str, DirectoryPlaylist]): Словарь плейлистов, где ключ - маршрут, значение - плейлист
        default_audio_directory (str): Директория по умолчанию
    """
    
    def __init__(self, default_audio_directory: str = None, directory_routes: Dict[str, str] = None):
        """
        Инициализирует менеджер плейлистов для разных маршрутов.
        
        Args:
            default_audio_directory (str, optional): Путь к директории с аудиофайлами по умолчанию
            directory_routes (Dict[str, str], optional): Словарь маршрутов и соответствующих им директорий
        """
        self.logger = logging.getLogger('audio_streamer')
        self.playlists = {}
        
        # Инициализируем плейлисты для каждой директории и маршрута
        if directory_routes:
            for route, directory in directory_routes.items():
                # Удаляем начальный слеш, если есть, для использования в качестве ключа
                route_key = route.lstrip('/')
                self.logger.info(f"Инициализация плейлиста для маршрута /{route_key} из директории {directory}")
                self.playlists[route_key] = DirectoryPlaylist(directory, self.logger)
        
        # Добавляем плейлист по умолчанию, если указана директория
        if default_audio_directory:
            # Ключ 'default' используется для маршрута по умолчанию ('/')
            self.logger.info(f"Инициализация плейлиста по умолчанию из директории {default_audio_directory}")
            self.playlists['default'] = DirectoryPlaylist(default_audio_directory, self.logger)
    
    def get_next_track(self, route: str = None) -> Optional[str]:
        """
        Возвращает путь к следующему аудиофайлу для указанного маршрута.
        
        Args:
            route (str, optional): Маршрут для которого нужно получить трек. 
                                   Если None, используется маршрут по умолчанию.
        
        Returns:
            Optional[str]: Путь к следующему аудиофайлу или None, если плейлист пуст
        """
        try:
            # Если маршрут начинается с /, убираем его для соответствия ключам словаря
            route_key = route.lstrip('/') if route else 'default'
            
            # Если плейлист для указанного маршрута не найден, используем плейлист по умолчанию
            if route_key not in self.playlists:
                self.logger.warning(f"Плейлист для маршрута /{route_key} не найден, используем плейлист по умолчанию")
                route_key = 'default'
            
            # Если плейлист по умолчанию тоже не найден, возвращаем None
            if route_key not in self.playlists:
                self.logger.error("Плейлист по умолчанию не найден")
                return None
            
            return self.playlists[route_key].get_next_track()
        
        except Exception as e:
            self.logger.error(f"Ошибка при получении следующего трека: {e}", exc_info=True)
            return None
    
    def get_available_routes(self) -> List[str]:
        """
        Возвращает список доступных маршрутов.
        
        Returns:
            List[str]: Список маршрутов
        """
        routes = []
        for route_key in self.playlists.keys():
            # Преобразуем ключ 'default' в маршрут '/'
            if route_key == 'default':
                routes.append('/')
            else:
                routes.append(f'/{route_key}')
        return routes
    
    def reload_playlist(self, route: str = None) -> None:
        """
        Перезагружает плейлист для указанного маршрута или все плейлисты.
        
        Args:
            route (str, optional): Маршрут, плейлист которого нужно перезагрузить.
                                  Если None, перезагружаются все плейлисты.
        """
        try:
            if route:
                # Если маршрут начинается с /, убираем его для соответствия ключам словаря
                route_key = route.lstrip('/')
                if route_key in self.playlists:
                    self.logger.info(f"Перезагрузка плейлиста для маршрута /{route_key}")
                    self.playlists[route_key].reload_playlist()
                elif 'default' in self.playlists:
                    self.logger.warning(f"Плейлист для маршрута /{route_key} не найден, перезагружаем плейлист по умолчанию")
                    self.playlists['default'].reload_playlist()
                else:
                    self.logger.error(f"Плейлист для маршрута /{route_key} и плейлист по умолчанию не найдены")
            else:
                # Перезагружаем все плейлисты
                self.logger.info("Перезагрузка всех плейлистов")
                for route_key, playlist in self.playlists.items():
                    route_name = '/' if route_key == 'default' else f'/{route_key}'
                    self.logger.info(f"Перезагрузка плейлиста для маршрута {route_name}")
                    playlist.reload_playlist()
        except Exception as e:
            self.logger.error(f"Ошибка при перезагрузке плейлиста: {e}", exc_info=True) 