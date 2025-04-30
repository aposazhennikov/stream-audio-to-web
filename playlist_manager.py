#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Модуль для работы с плейлистом аудиофайлов.
"""

import os
import random
import logging
import stat
import threading
import time
import io
from typing import List, Optional, Dict, Tuple, BinaryIO
from collections import deque


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
    
    def get_sequential_playlist(self) -> List[str]:
        """
        Возвращает последовательный список аудиофайлов (без перемешивания).
        
        Returns:
            List[str]: Отсортированный список путей к аудиофайлам
        """
        # Сортируем по имени файла для предсказуемого порядка
        sorted_files = sorted(self.audio_files, key=os.path.basename)
        return sorted_files
    
    def reload_playlist(self) -> None:
        """
        Перезагружает плейлист, сканируя директорию заново.
        """
        try:
            self.current_index = 0
            self._scan_directory()
        except Exception as e:
            self.logger.error(f"Ошибка при перезагрузке плейлиста: {e}", exc_info=True)


class AudioStreamBuffer:
    """
    Класс для буферизации аудиопотока и обеспечения синхронизации слушателей.
    
    Attributes:
        buffer (deque): Кольцевой буфер для хранения аудиоданных
        buffer_size (int): Максимальный размер буфера в байтах
        current_position (int): Текущая позиция в аудиопотоке
        access_lock (threading.Lock): Блокировка для многопоточного доступа
    """
    
    def __init__(self, buffer_size: int = 10 * 1024 * 1024):  # 10MB по умолчанию
        """
        Инициализирует буфер аудиопотока.
        
        Args:
            buffer_size (int): Максимальный размер буфера в байтах
        """
        self.buffer = deque()
        self.buffer_size = buffer_size
        self.current_size = 0
        self.current_position = 0
        self.access_lock = threading.Lock()
        self.logger = logging.getLogger('audio_streamer')
    
    def add_chunk(self, chunk: bytes) -> None:
        """
        Добавляет чанк аудиоданных в буфер.
        
        Args:
            chunk (bytes): Чанк аудиоданных
        """
        with self.access_lock:
            # Добавляем чанк в буфер
            self.buffer.append(chunk)
            chunk_size = len(chunk)
            self.current_size += chunk_size
            self.current_position += chunk_size
            
            # Удаляем старые данные, если буфер переполнен
            while self.current_size > self.buffer_size and self.buffer:
                old_chunk = self.buffer.popleft()
                self.current_size -= len(old_chunk)
    
    def get_chunks(self, start_pos: int = None) -> Tuple[List[bytes], int]:
        """
        Получает все доступные чанки аудиоданных с указанной позиции.
        
        Args:
            start_pos (int, optional): Начальная позиция для чтения. 
                                      Если None, возвращаются все данные из буфера.
        
        Returns:
            Tuple[List[bytes], int]: Список чанков аудиоданных и новая позиция
        """
        with self.access_lock:
            if start_pos is None or start_pos < (self.current_position - self.current_size):
                # Если позиция не указана или слишком старая, возвращаем все данные из буфера
                return list(self.buffer), self.current_position
            
            # Количество байт, которые нужно пропустить (старые данные)
            skip_bytes = (self.current_position - self.current_size) - start_pos
            
            if skip_bytes >= 0:
                # Если позиция в пределах буфера, находим соответствующие чанки
                current_skipped = 0
                start_index = 0
                
                for i, chunk in enumerate(self.buffer):
                    chunk_size = len(chunk)
                    if current_skipped + chunk_size > skip_bytes:
                        # Нашли начальный чанк
                        start_index = i
                        break
                    current_skipped += chunk_size
                
                # Возвращаем чанки с указанного индекса
                return list(self.buffer)[start_index:], self.current_position
            else:
                # Если позиция в будущем (что странно), возвращаем пустой список
                return [], self.current_position
    
    def get_current_position(self) -> int:
        """
        Возвращает текущую позицию в аудиопотоке.
        
        Returns:
            int: Текущая позиция в аудиопотоке (в байтах)
        """
        with self.access_lock:
            return self.current_position
    
    def reset(self) -> None:
        """
        Сбрасывает буфер.
        """
        with self.access_lock:
            self.buffer.clear()
            self.current_size = 0
            self.current_position = 0


class RadioStream:
    """
    Класс для создания единого непрерывного радиопотока для определенного маршрута.
    
    Attributes:
        route (str): Маршрут потока
        directory (str): Путь к директории с аудиофайлами
        buffer (AudioStreamBuffer): Буфер для хранения аудиоданных
        is_running (bool): Флаг работы потока
        current_track (str): Текущий проигрываемый трек
    """
    
    def __init__(self, route: str, directory: str, audio_streamer, logger):
        """
        Инициализирует радиопоток для маршрута.
        
        Args:
            route (str): Маршрут потока
            directory (str): Путь к директории с аудиофайлами
            audio_streamer: Объект для создания аудиопотоков
            logger: Логгер для записи сообщений
        """
        self.route = route
        self.directory = directory
        self.audio_streamer = audio_streamer
        self.logger = logger
        
        # Создаем плейлист для директории
        self.playlist = DirectoryPlaylist(directory, logger)
        
        # Буфер для аудиоданных
        self.buffer = AudioStreamBuffer()
        
        # Флаги и объекты для многопоточности
        self.is_running = False
        self.thread = None
        self.stop_event = threading.Event()
        
        # Информация о текущем треке
        self.current_track = None
        self.next_tracks_queue = deque()
        
        # Предзагрузка треков
        self.preload_queue = deque()
        self.preload_thread = None
        self.preload_lock = threading.Lock()
        self.preload_event = threading.Event()
        
        # Статистика
        self.listeners_count = 0
        self.total_bytes_streamed = 0
        self.stream_start_time = None
    
    def start(self) -> None:
        """
        Запускает радиопоток.
        """
        if self.is_running:
            return
        
        self.is_running = True
        self.stop_event.clear()
        self.stream_start_time = time.time()
        
        # Запускаем поток для предзагрузки треков
        self.preload_event.clear()
        self.preload_thread = threading.Thread(target=self._preload_tracks_worker, daemon=True)
        self.preload_thread.start()
        
        # Запускаем поток для воспроизведения
        self.thread = threading.Thread(target=self._stream_worker, daemon=True)
        self.thread.start()
        
        self.logger.info(f"Радиопоток для маршрута {self.route} запущен")
    
    def stop(self) -> None:
        """
        Останавливает радиопоток.
        """
        if not self.is_running:
            return
        
        self.is_running = False
        self.stop_event.set()
        self.preload_event.set()
        
        if self.thread and self.thread.is_alive():
            self.thread.join(timeout=2.0)
        
        if self.preload_thread and self.preload_thread.is_alive():
            self.preload_thread.join(timeout=2.0)
        
        self.buffer.reset()
        self.logger.info(f"Радиопоток для маршрута {self.route} остановлен")
    
    def _preload_tracks_worker(self) -> None:
        """
        Рабочий поток для предзагрузки треков.
        """
        while not self.stop_event.is_set():
            try:
                # Загружаем треки в очередь предзагрузки, если она почти пуста
                with self.preload_lock:
                    if len(self.preload_queue) < 3:  # Предзагружаем до 3 треков
                        tracks_to_load = 3 - len(self.preload_queue)
                        for _ in range(tracks_to_load):
                            track = self.playlist.get_next_track()
                            if track:
                                self.preload_queue.append(track)
                                self.logger.info(f"Предзагрузка трека для {self.route}: {os.path.basename(track)}")
                
                # Ждем сигнала или таймаута
                self.preload_event.wait(timeout=5.0)
                self.preload_event.clear()
                
            except Exception as e:
                self.logger.error(f"Ошибка в потоке предзагрузки для {self.route}: {e}", exc_info=True)
                time.sleep(1.0)  # Пауза при ошибке
    
    def _stream_worker(self) -> None:
        """
        Рабочий поток для потоковой передачи аудио.
        """
        chunk_size = 4096  # Размер чанка аудио
        
        while not self.stop_event.is_set():
            try:
                # Получаем следующий трек из очереди предзагрузки
                next_track = None
                with self.preload_lock:
                    if self.preload_queue:
                        next_track = self.preload_queue.popleft()
                        self.preload_event.set()  # Сигнал для загрузки следующего трека
                
                # Если нет предзагруженных треков, берем напрямую из плейлиста
                if not next_track:
                    next_track = self.playlist.get_next_track()
                
                if not next_track:
                    self.logger.warning(f"Нет доступных треков для {self.route}, ожидание...")
                    time.sleep(2.0)
                    continue
                
                self.current_track = next_track
                file_name = os.path.basename(next_track)
                self.logger.info(f"Начало трансляции трека на маршруте {self.route}: {file_name}")
                
                # Создаем аудиопоток
                audio_stream, _ = self.audio_streamer.create_stream_from_file(next_track)
                if not audio_stream:
                    self.logger.error(f"Не удалось создать аудиопоток для файла: {file_name}")
                    continue
                
                try:
                    # Читаем аудиоданные из файла и добавляем в буфер
                    while not self.stop_event.is_set():
                        chunk = audio_stream.read(chunk_size)
                        if not chunk:
                            break
                        
                        # Добавляем чанк в буфер
                        self.buffer.add_chunk(chunk)
                        self.total_bytes_streamed += len(chunk)
                        
                        # Маленькая пауза для предотвращения перегрузки CPU
                        time.sleep(0.001)
                    
                    self.logger.info(f"Трек завершен: {file_name}")
                    
                finally:
                    # Закрываем аудиопоток
                    if audio_stream:
                        try:
                            audio_stream.close()
                        except Exception as e:
                            self.logger.warning(f"Ошибка при закрытии аудиопотока: {e}")
            
            except Exception as e:
                self.logger.error(f"Ошибка в потоке аудиотрансляции для {self.route}: {e}", exc_info=True)
                time.sleep(1.0)  # Пауза при ошибке
    
    def get_stream_data(self, position: int = None) -> Tuple[List[bytes], int]:
        """
        Получает данные аудиопотока с указанной позиции.
        
        Args:
            position (int, optional): Начальная позиция для чтения.
                                     Если None, возвращаются все данные из буфера.
        
        Returns:
            Tuple[List[bytes], int]: Список чанков аудиоданных и новая позиция
        """
        return self.buffer.get_chunks(position)
    
    def get_current_position(self) -> int:
        """
        Возвращает текущую позицию в аудиопотоке.
        
        Returns:
            int: Текущая позиция (в байтах)
        """
        return self.buffer.get_current_position()
    
    def get_current_track(self) -> Optional[str]:
        """
        Возвращает текущий проигрываемый трек.
        
        Returns:
            Optional[str]: Путь к текущему треку или None
        """
        return self.current_track
    
    def add_listener(self) -> None:
        """
        Регистрирует нового слушателя.
        """
        self.listeners_count += 1
        self.logger.info(f"Новый слушатель для {self.route}. Всего слушателей: {self.listeners_count}")
    
    def remove_listener(self) -> None:
        """
        Удаляет слушателя.
        """
        if self.listeners_count > 0:
            self.listeners_count -= 1
            self.logger.info(f"Слушатель отключился от {self.route}. Осталось слушателей: {self.listeners_count}")


class PlaylistManager:
    """
    Класс для управления несколькими плейлистами, отображенных на разные маршруты.
    
    Attributes:
        playlists (Dict[str, DirectoryPlaylist]): Словарь плейлистов, где ключ - маршрут, значение - плейлист
        radio_streams (Dict[str, RadioStream]): Словарь радиопотоков для каждого маршрута
        default_audio_directory (str): Директория по умолчанию
        stream_mode (str): Режим работы плейлиста ('individual' или 'radio')
    """
    
    def __init__(self, default_audio_directory: str = None, directory_routes: Dict[str, str] = None, 
                 stream_mode: str = 'radio'):
        """
        Инициализирует менеджер плейлистов для разных маршрутов.
        
        Args:
            default_audio_directory (str, optional): Путь к директории с аудиофайлами по умолчанию
            directory_routes (Dict[str, str], optional): Словарь маршрутов и соответствующих им директорий
            stream_mode (str): Режим работы плейлиста ('individual' или 'radio')
        """
        self.logger = logging.getLogger('audio_streamer')
        self.playlists = {}
        self.radio_streams = {}
        self.default_audio_directory = default_audio_directory
        self.stream_mode = stream_mode
        
        # Импортируем AudioStreamer здесь для предотвращения циклических импортов
        from audio_streamer import AudioStreamer
        self.audio_streamer = AudioStreamer()
        
        # Инициализируем плейлисты для каждой директории и маршрута
        if directory_routes:
            for route, directory in directory_routes.items():
                # Удаляем начальный слеш, если есть, для использования в качестве ключа
                route_key = route.lstrip('/')
                self.logger.info(f"Инициализация плейлиста для маршрута /{route_key} из директории {directory}")
                
                # Инициализируем плейлист для индивидуального режима
                self.playlists[route_key] = DirectoryPlaylist(directory, self.logger)
                
                # Инициализируем и запускаем радиопоток в радиорежиме
                if self.stream_mode == 'radio':
                    radio_route = '/' + route_key if route_key else '/'
                    radio_stream = RadioStream(radio_route, directory, self.audio_streamer, self.logger)
                    radio_stream.start()
                    self.radio_streams[radio_route] = radio_stream
        
        # Добавляем плейлист по умолчанию, если указана директория
        if default_audio_directory:
            # Ключ 'default' используется для маршрута по умолчанию ('/')
            self.logger.info(f"Инициализация плейлиста по умолчанию из директории {default_audio_directory}")
            self.playlists['default'] = DirectoryPlaylist(default_audio_directory, self.logger)
            
            # Инициализируем и запускаем радиопоток в радиорежиме
            if self.stream_mode == 'radio':
                radio_stream = RadioStream('/', default_audio_directory, self.audio_streamer, self.logger)
                radio_stream.start()
                self.radio_streams['/'] = radio_stream
    
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
            
            # Если мы в режиме радио, возвращаем текущий трек из радиопотока
            if self.stream_mode == 'radio':
                radio_route = '/' + route_key if route_key else '/'
                if radio_route in self.radio_streams:
                    current_track = self.radio_streams[radio_route].get_current_track()
                    if current_track:
                        return current_track
            
            # Если радиопоток недоступен или в индивидуальном режиме, используем обычный плейлист
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
    
    def get_radio_stream(self, route: str = '/') -> Optional[RadioStream]:
        """
        Возвращает радиопоток для указанного маршрута.
        
        Args:
            route (str): Маршрут, для которого нужно получить радиопоток
            
        Returns:
            Optional[RadioStream]: Радиопоток или None
        """
        if self.stream_mode != 'radio':
            return None
        
        if route in self.radio_streams:
            return self.radio_streams[route]
        
        # Если маршрут не найден, возвращаем поток по умолчанию
        if '/' in self.radio_streams:
            return self.radio_streams['/']
        
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
                    
                    # Перезапускаем соответствующий радиопоток
                    radio_route = '/' + route_key if route_key else '/'
                    if radio_route in self.radio_streams:
                        self.logger.info(f"Перезапуск радиопотока для маршрута {radio_route}")
                        self.radio_streams[radio_route].stop()
                        time.sleep(1.0)
                        self.radio_streams[radio_route].start()
                        
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
                    
                    # Перезапускаем все радиопотоки
                    if self.stream_mode == 'radio':
                        for radio_route, radio_stream in self.radio_streams.items():
                            self.logger.info(f"Перезапуск радиопотока для маршрута {radio_route}")
                            radio_stream.stop()
                            time.sleep(1.0)
                            radio_stream.start()
        except Exception as e:
            self.logger.error(f"Ошибка при перезагрузке плейлиста: {e}", exc_info=True)
    
    def shutdown(self) -> None:
        """
        Останавливает все радиопотоки и освобождает ресурсы.
        """
        if self.stream_mode == 'radio':
            for route, stream in self.radio_streams.items():
                self.logger.info(f"Остановка радиопотока для маршрута {route}")
                stream.stop() 