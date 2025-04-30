#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Модуль веб-сервера для потоковой передачи аудио.
"""

import os
import logging
import threading
import time
import sys
import json
from typing import Dict, Generator, Optional, List, Tuple
from flask import Flask, Response, render_template_string, stream_with_context, jsonify, redirect, request
from werkzeug.exceptions import BadRequest
from werkzeug.serving import run_simple
from playlist_manager import PlaylistManager
from audio_streamer import AudioStreamer


HTML_TEMPLATE = """
<!DOCTYPE html>
<html>
<head>
    <title>Аудио стриминг</title>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <style>
        body {
            font-family: Arial, sans-serif;
            margin: 0;
            padding: 20px;
            background-color: #f4f4f4;
            color: #333;
        }
        .container {
            max-width: 800px;
            margin: 0 auto;
            background-color: white;
            padding: 20px;
            border-radius: 5px;
            box-shadow: 0 2px 5px rgba(0,0,0,0.1);
        }
        h1 {
            color: #2c3e50;
            text-align: center;
        }
        .player-container {
            margin: 20px 0;
            text-align: center;
        }
        audio {
            width: 100%;
            margin: 20px 0;
        }
        .now-playing {
            margin: 20px 0;
            font-style: italic;
            color: #7f8c8d;
            text-align: center;
            min-height: 20px;
            padding: 10px;
            background-color: #ecf0f1;
            border-radius: 5px;
        }
        #track-name {
            font-weight: bold;
            color: #3498db;
            display: block;
            margin-top: 5px;
            font-size: 1.2em;
        }
        .stream-list {
            margin: 20px 0;
            padding: 0;
            list-style-type: none;
        }
        .stream-list li {
            margin: 10px 0;
            padding: 10px;
            background-color: #ecf0f1;
            border-radius: 5px;
        }
        .stream-list a {
            color: #3498db;
            text-decoration: none;
            font-weight: bold;
        }
        .stream-list a:hover {
            text-decoration: underline;
        }
        .stream-source {
            color: #7f8c8d;
            font-size: 0.9em;
            margin-top: 5px;
        }
    </style>
    <script>
        // Периодическое обновление информации о текущем треке
        function updateCurrentTrack() {
            // Получаем текущий маршрут из URL
            var streamRoute = window.location.pathname.replace('/web', '');
            if (!streamRoute) streamRoute = '/';
            
            fetch('/now-playing' + streamRoute)
                .then(response => response.json())
                .then(data => {
                    if (data.track) {
                        document.getElementById('track-name').textContent = data.track;
                    }
                })
                .catch(err => console.error('Ошибка при получении информации о треке:', err));
        }
        
        window.onload = function() {
            // Первоначальное обновление информации о треке
            updateCurrentTrack();
            
            // Периодическое обновление каждые 5 секунд
            setInterval(updateCurrentTrack, 5000);
        };
    </script>
</head>
<body>
    <div class="container">
        <h1>Аудио стриминг</h1>
        <div class="player-container">
            <div class="now-playing">
                Сейчас играет: 
                <span id="track-name">загрузка...</span>
            </div>
            
            <audio id="audio-player" controls autoplay>
                <source src="/stream" type="audio/mpeg">
                Ваш браузер не поддерживает аудио элемент.
            </audio>
        </div>
    </div>
</body>
</html>
"""


class AudioStreamServer:
    """
    Класс веб-сервера для потоковой передачи аудио.
    
    Attributes:
        playlist_managers (Dict[str, PlaylistManager]): Словарь менеджеров плейлистов для разных маршрутов
        audio_streamer (AudioStreamer): Обработчик аудиопотока
        app (Flask): Экземпляр Flask приложения
        current_tracks (Dict[str, str]): Словарь текущих проигрываемых треков для каждого маршрута
    """
    
    def __init__(self, folder_route_mappings: Dict[str, str] = None):
        """
        Инициализирует веб-сервер для потоковой передачи аудио.
        
        Args:
            folder_route_mappings (Dict[str, str], optional): Словарь соответствия папок к маршрутам 
                в формате {'/путь/к/папке': '/маршрут'}, например {'/app/humor': '/humor'}
        """
        self.logger = logging.getLogger('audio_streamer')
        self.audio_streamer = AudioStreamer()
        self.app = Flask(__name__)
        
        # Словарь для хранения текущих треков и менеджеров плейлистов по маршрутам
        self.current_tracks = {}
        self.playlist_managers = {}
        
        # Создаем карту маршрутов и папок
        self.route_mappings = self._process_folder_route_mappings(folder_route_mappings or {})
        
        if not self.route_mappings:
            # Если нет соответствия папка-маршрут, используем режим совместимости
            # (один плейлист, несколько маршрутов)
            legacy_audio_dir = os.environ.get('AUDIO_DIR', '/app/audio')
            self.stream_routes = self._get_stream_routes()
            self.logger.info(f"Работа в режиме совместимости: одна папка - несколько маршрутов")
            self.logger.info(f"Аудио директория: {legacy_audio_dir}")
            self.logger.info(f"Маршруты: {', '.join(self.stream_routes)}")
            
            # Создаем один менеджер плейлиста для всех маршрутов
            playlist_manager = PlaylistManager(legacy_audio_dir)
            for route in self.stream_routes:
                self.playlist_managers[route] = playlist_manager
                self.current_tracks[route] = None
        
        # Выводим информацию о настроенных маршрутах
        self.logger.info(f"Настроены следующие маршруты для аудиопотока:")
        for route, audio_dir in self.route_mappings.items():
            self.logger.info(f"  Маршрут: {route} -> Папка: {audio_dir}")
        
        # Регистрация маршрутов и обработчиков ошибок
        self._register_routes()
        self._register_error_handlers()
    
    def _process_folder_route_mappings(self, folder_route_mappings: Dict[str, str]) -> Dict[str, str]:
        """
        Обрабатывает соответствия папок к маршрутам и создает менеджеры плейлистов.
        
        Args:
            folder_route_mappings (Dict[str, str]): Словарь соответствия папок к маршрутам

        Returns:
            Dict[str, str]: Словарь соответствия маршрутов к папкам (обратное отображение)
        """
        # Преобразуем в формат {маршрут: папка} для удобства
        route_to_folder = {}
        
        for folder_path, route in folder_route_mappings.items():
            # Убедимся, что маршрут начинается с '/'
            if not route.startswith('/'):
                route = f'/{route}'
            
            # Создаем менеджер плейлиста для этой папки
            try:
                self.logger.info(f"Инициализация плейлиста для папки {folder_path} (маршрут {route})")
                
                # Проверяем существование директории
                if not os.path.exists(folder_path):
                    self.logger.warning(f"Директория {folder_path} не найдена, создаем...")
                    os.makedirs(folder_path, exist_ok=True)
                
                # Создаем менеджер плейлиста
                playlist_manager = PlaylistManager(folder_path)
                self.playlist_managers[route] = playlist_manager
                self.current_tracks[route] = None
                
                # Добавляем соответствие маршрут -> папка
                route_to_folder[route] = folder_path
            except Exception as e:
                self.logger.error(f"Ошибка при инициализации плейлиста для {folder_path}: {e}", exc_info=True)
        
        return route_to_folder
    
    def _get_stream_routes(self) -> List[str]:
        """
        Получает список маршрутов для аудиопотока из переменной окружения.
        
        Returns:
            List[str]: Список URL-путей для прямого аудиопотока
        """
        # Получаем пути из переменных окружения или используем значения по умолчанию
        routes_str = os.environ.get('AUDIO_STREAM_ROUTES', '/')
        
        # Разделяем строку с путями по запятой и очищаем от пробелов
        routes = [route.strip() for route in routes_str.split(',')]
        
        # Убеждаемся, что каждый путь начинается с '/'
        routes = [route if route.startswith('/') else f'/{route}' for route in routes]
        
        # Делаем список уникальным
        return list(set(routes))
    
    def _register_routes(self) -> None:
        """
        Регистрирует маршруты Flask для веб-сервера.
        """
        try:
            # Динамическая регистрация маршрутов для аудиопотока
            if self.route_mappings:
                # Для режима разных папок по маршрутам
                for route in self.route_mappings.keys():
                    self._register_stream_route(route)
            else:
                # Для режима совместимости (один плейлист, несколько маршрутов)
                for route in self.stream_routes:
                    self._register_stream_route(route)
            
            @self.app.route('/web')
            def web_interface():
                """Основной веб-интерфейс с выбором стримов."""
                return self._generate_web_interface()
            
            @self.app.route('/web/<path:route>')
            def stream_web_interface(route):
                """Веб-интерфейс для конкретного стрима."""
                if not route.startswith('/'):
                    route = f'/{route}'
                return self._generate_web_interface(route)
            
            @self.app.route('/stream')
            def default_stream():
                """Эндпоинт для потоковой передачи аудио через веб-интерфейс по умолчанию."""
                default_route = next(iter(self.playlist_managers.keys()), '/')
                return Response(
                    stream_with_context(self._generate_audio_stream(default_route)),
                    mimetype='audio/mpeg'
                )
            
            @self.app.route('/stream/<path:route>')
            def specific_stream(route):
                """Эндпоинт для потоковой передачи аудио из конкретного маршрута."""
                if not route.startswith('/'):
                    route = f'/{route}'
                return Response(
                    stream_with_context(self._generate_audio_stream(route)),
                    mimetype='audio/mpeg'
                )
                
            @self.app.route('/direct-stream')
            def direct_stream():
                """Прямой аудиопоток для радиоприемников (legacy URL)."""
                default_route = next(iter(self.playlist_managers.keys()), '/')
                return self._create_direct_stream_response(default_route)
                
            @self.app.route('/now-playing')
            def default_now_playing():
                """Информация о текущем треке по умолчанию."""
                default_route = next(iter(self.playlist_managers.keys()), '/')
                return self._get_now_playing_info(default_route)
            
            @self.app.route('/now-playing/<path:route>')
            def specific_now_playing(route):
                """Информация о текущем треке для конкретного маршрута."""
                if not route.startswith('/'):
                    route = f'/{route}'
                return self._get_now_playing_info(route)
            
            @self.app.route('/reload-playlist')
            def reload_all_playlists():
                """Эндпоинт для перезагрузки всех плейлистов."""
                try:
                    for route, manager in self.playlist_managers.items():
                        self.logger.info(f"Перезагрузка плейлиста для маршрута {route}")
                        manager.reload_playlist()
                    return jsonify({"status": "success", "message": "Все плейлисты перезагружены"})
                except Exception as e:
                    self.logger.error(f"Ошибка при перезагрузке плейлистов: {e}", exc_info=True)
                    return jsonify({"status": "error", "message": str(e)})
            
            @self.app.route('/reload-playlist/<path:route>')
            def reload_specific_playlist(route):
                """Эндпоинт для перезагрузки конкретного плейлиста."""
                if not route.startswith('/'):
                    route = f'/{route}'
                
                if route not in self.playlist_managers:
                    return jsonify({"status": "error", "message": f"Маршрут {route} не найден"}), 404
                
                try:
                    self.logger.info(f"Перезагрузка плейлиста для маршрута {route}")
                    self.playlist_managers[route].reload_playlist()
                    return jsonify({"status": "success", "message": f"Плейлист для маршрута {route} перезагружен"})
                except Exception as e:
                    self.logger.error(f"Ошибка при перезагрузке плейлиста для {route}: {e}", exc_info=True)
                    return jsonify({"status": "error", "message": str(e)})
            
            # Добавляем маршрут для списка доступных аудиопотоков
            @self.app.route('/streams')
            def list_streams():
                """Эндпоинт для получения списка доступных аудиопотоков."""
                streams = []
                
                host_url = request.host_url.rstrip('/')
                
                for route in self.playlist_managers.keys():
                    folder = self.route_mappings.get(route, "Стандартная папка аудио")
                    streams.append({
                        "route": route,
                        "url": f"{host_url}{route}",
                        "web_interface": f"{host_url}/web{route}",
                        "source_folder": folder
                    })
                
                return jsonify({
                    "status": "success", 
                    "message": "Список доступных аудиопотоков",
                    "streams": streams
                })
        
        except Exception as e:
            self.logger.error(f"Ошибка при регистрации маршрутов: {e}", exc_info=True)
    
    def _register_stream_route(self, route: str) -> None:
        """
        Регистрирует маршрут для аудиопотока.
        
        Args:
            route (str): URL-путь для прямого аудиопотока
        """
        # Создаем уникальное имя для функции для каждого маршрута
        route_name = f"audio_stream_{route.replace('/', '_').strip('_')}"
        if not route_name:
            route_name = "audio_stream_root"
        
        # Определяем функцию для обработки запроса
        def stream_handler():
            self.logger.info(f"Запрос на аудиопоток через маршрут: {route}")
            return self._create_direct_stream_response(route)
        
        # Переименовываем функцию для корректной регистрации в Flask
        stream_handler.__name__ = route_name
        
        # Регистрируем функцию как обработчик маршрута
        self.app.add_url_rule(route, route_name, stream_handler)
        self.logger.info(f"Зарегистрирован маршрут для аудиопотока: {route}")
    
    def _register_error_handlers(self) -> None:
        """
        Регистрирует обработчики ошибок Flask для веб-сервера.
        """
        try:
            @self.app.errorhandler(400)
            def handle_bad_request(e):
                """
                Обработчик ошибок 400 (Bad Request).
                Отлавливает ошибки от TLS-соединений к HTTP-серверу.
                """
                # Проверяем есть ли бинарные данные в описании ошибки
                error_desc = str(e)
                if '\\x' in error_desc:
                    self.logger.warning(f"Перехвачена попытка TLS-соединения: {str(e)[:30]}...")
                    # Возвращаем простой ответ вместо стандартной ошибки 400
                    return Response("Это HTTP-сервер. Используйте HTTP, а не HTTPS.", 
                                    status=400, 
                                    mimetype='text/plain')
                return e
            
            @self.app.errorhandler(404)
            def handle_not_found(e):
                """Обработчик ошибок 404 (Not Found)."""
                self.logger.warning(f"Запрос к несуществующему ресурсу: {request.path}")
                return Response(
                    "Запрашиваемый ресурс не найден. Проверьте URL или посетите /streams для списка доступных потоков.",
                    status=404,
                    mimetype='text/plain'
                )
        
        except Exception as e:
            self.logger.error(f"Ошибка при регистрации обработчиков ошибок: {e}", exc_info=True)
    
    def _generate_web_interface(self, active_route: str = None) -> str:
        """
        Генерирует веб-интерфейс с учетом активного маршрута.
        
        Args:
            active_route (str): Активный маршрут для отображения в интерфейсе
        
        Returns:
            str: HTML-страница веб-интерфейса
        """
        # Если не указан активный маршрут, используем первый доступный
        if not active_route:
            active_route = next(iter(self.playlist_managers.keys()), '/')
        
        # Получаем список доступных аудиопотоков для отображения
        streams = []
        for route, manager in self.playlist_managers.items():
            source_folder = self.route_mappings.get(route, "Стандартная аудио папка")
            streams.append({
                "route": route,
                "url": f"/web{route}",
                "source_folder": source_folder,
                "active": route == active_route
            })
        
        # Генерируем HTML со списком доступных потоков
        streams_html = ""
        if len(streams) > 1:
            streams_html = "<h3>Доступные аудиопотоки:</h3><ul class='stream-list'>"
            for stream in streams:
                active_class = " active" if stream["active"] else ""
                streams_html += f"<li class='{active_class}'>"
                streams_html += f"<a href='{stream['url']}'>{stream['route']}</a>"
                streams_html += f"<div class='stream-source'>Источник: {stream['source_folder']}</div>"
                streams_html += "</li>"
            streams_html += "</ul>"
        
        # Адаптируем HTML-шаблон
        html = HTML_TEMPLATE.replace(
            '<source src="/stream" type="audio/mpeg">',
            f'<source src="/stream{active_route}" type="audio/mpeg">'
        )
        
        # Добавляем список потоков перед закрывающим div.container
        if streams_html:
            html = html.replace('</div>\n</body>', f'{streams_html}</div>\n</body>')
        
        return html
    
    def _get_now_playing_info(self, route: str):
        """
        Возвращает информацию о текущем треке для указанного маршрута.
        
        Args:
            route (str): Маршрут аудиопотока
            
        Returns:
            Response: JSON-ответ с информацией о треке
        """
        if route not in self.playlist_managers:
            return jsonify({
                "status": "error",
                "message": f"Маршрут {route} не найден",
                "track": "Нет данных"
            }), 404
        
        track_name = "Нет активного трека"
        if self.current_tracks.get(route):
            track_name = os.path.basename(self.current_tracks[route])
        
        # Получаем информацию о папке-источнике
        source_folder = self.route_mappings.get(route, "Стандартная папка аудио")
        
        return jsonify({
            "status": "success",
            "route": route,
            "source_folder": source_folder,
            "track": track_name
        })
    
    def _create_direct_stream_response(self, route: str):
        """
        Создает ответ для прямого аудиопотока для указанного маршрута
        
        Args:
            route (str): Маршрут аудиопотока
            
        Returns:
            Response: Flask-ответ с аудиопотоком и необходимыми заголовками
        """
        if route not in self.playlist_managers:
            self.logger.error(f"Попытка доступа к несуществующему маршруту: {route}")
            return Response(
                "Указанный аудиопоток не найден",
                status=404,
                mimetype='text/plain'
            )
        
        # Получаем информацию о папке-источнике для названия потока
        source_folder = self.route_mappings.get(route, "Audio Stream")
        stream_name = f"Audio Stream - {route}"
        
        return Response(
            stream_with_context(self._generate_audio_stream(route)),
            mimetype='audio/mpeg',
            headers={
                # Заголовки для совместимости с радиоприемниками и автоматического воспроизведения
                'Content-Type': 'audio/mpeg',
                'icy-name': stream_name,
                'icy-description': f'Direct audio stream from {source_folder}',
                'icy-genre': 'Various',
                'icy-br': '192',
                'Cache-Control': 'no-cache, no-store, must-revalidate',
                'Pragma': 'no-cache',
                'Expires': '0',
                'X-Content-Type-Options': 'nosniff'
            }
        )
    
    def _generate_audio_stream(self, route: str) -> Generator[bytes, None, None]:
        """
        Генератор для потоковой передачи аудио для указанного маршрута.
        
        Args:
            route (str): Маршрут аудиопотока
            
        Yields:
            bytes: Чанки аудиоданных
        """
        if route not in self.playlist_managers:
            self.logger.error(f"Попытка генерации потока для несуществующего маршрута: {route}")
            yield b''
            return
        
        try:
            playlist_manager = self.playlist_managers[route]
            
            while True:
                track = playlist_manager.get_next_track()
                if not track:
                    # Если нет треков, ждем и пробуем снова
                    self.logger.warning(f"Нет доступных треков для воспроизведения по маршруту {route}, ожидание...")
                    time.sleep(2)
                    continue
                
                self.current_tracks[route] = track
                file_name = os.path.basename(track)
                self.logger.info(f"Начало трансляции трека по маршруту {route}: {file_name}")
                
                audio_stream, _ = self.audio_streamer.create_stream_from_file(track)
                if not audio_stream:
                    self.logger.error(f"Не удалось создать аудиопоток для файла: {file_name}")
                    continue
                
                try:
                    while True:
                        chunk = audio_stream.read(self.audio_streamer.chunk_size)
                        if not chunk:
                            break
                        yield chunk
                    
                    # Закрытие потока
                    audio_stream.close()
                except Exception as e:
                    self.logger.error(f"Ошибка при чтении аудиопотока: {e}", exc_info=True)
                    if audio_stream:
                        try:
                            audio_stream.close()
                        except:
                            pass
        
        except Exception as e:
            self.logger.error(f"Ошибка при генерации аудиопотока для маршрута {route}: {e}", exc_info=True)
            # Возвращаем пустые данные, чтобы не прерывать поток
            yield b''
    
    def run(self, host: str = '0.0.0.0', port: int = 9999, debug: bool = False) -> None:
        """
        Запускает веб-сервер для потоковой передачи аудио.
        
        Args:
            host (str): IP-адрес сервера (по умолчанию '0.0.0.0')
            port (int): Порт сервера (по умолчанию 9999)
            debug (bool): Флаг режима отладки Flask (по умолчанию False)
        """
        try:
            self.logger.info(f"Запуск аудио-сервера на {host}:{port}")
            
            # Используем производственный сервер вместо встроенного development сервера
            run_simple(
                hostname=host,
                port=port,
                application=self.app,
                threaded=True,
                use_reloader=debug,
                use_debugger=debug,
                # Выключаем предупреждение о development сервере
                use_evalex=False
            )
        except Exception as e:
            self.logger.error(f"Ошибка при запуске аудио-сервера: {e}", exc_info=True)


def parse_folder_route_map(map_str: str) -> Dict[str, str]:
    """
    Парсит строку с соответствиями папок и маршрутов.
    
    Args:
        map_str (str): Строка в формате "путь1:маршрут1,путь2:маршрут2"
        
    Returns:
        Dict[str, str]: Словарь соответствия папок к маршрутам
    """
    if not map_str:
        return {}
    
    result = {}
    try:
        # Разбиваем строку на пары
        pairs = map_str.split(',')
        for pair in pairs:
            if ':' in pair:
                folder_path, route = pair.strip().split(':', 1)
                folder_path = folder_path.strip()
                route = route.strip()
                
                if folder_path and route:
                    result[folder_path] = route
    except Exception as e:
        logging.error(f"Ошибка при парсинге карты папок и маршрутов: {e}", exc_info=True)
    
    return result


# Если запускается непосредственно этот файл, создаем и запускаем WSGI-приложение
def create_app(folder_route_map: Dict[str, str] = None):
    """
    Создает Flask приложение для использования с WSGI-серверами
    
    Args:
        folder_route_map (Dict[str, str], optional): Словарь соответствия папок к маршрутам
        
    Returns:
        Flask: Flask-приложение
    """
    # Пытаемся получить карту папок и маршрутов из переменной окружения
    env_map_str = os.environ.get('FOLDER_ROUTE_MAP', '')
    env_map = parse_folder_route_map(env_map_str)
    
    # Если передана карта в аргументах, она имеет приоритет
    folder_route_map = folder_route_map or env_map
    
    server = AudioStreamServer(folder_route_map)
    return server.app

# Эта функция позволит использовать WSGI-серверы вроде Gunicorn
application = create_app() 