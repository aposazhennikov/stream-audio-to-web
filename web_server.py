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
import socket
from typing import Generator, Optional, List, Dict
from flask import Flask, Response, render_template_string, stream_with_context, jsonify, redirect, request, abort
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
    </style>
    <script>
        // Периодическое обновление информации о текущем треке
        function updateCurrentTrack() {
            fetch('/now-playing')
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
        playlist_manager (PlaylistManager): Менеджер плейлиста
        audio_streamer (AudioStreamer): Обработчик аудиопотока
        app (Flask): Экземпляр Flask приложения
        current_tracks (Dict[str, str]): Словарь текущих треков для каждого маршрута
    """
    
    def __init__(self, playlist_manager: PlaylistManager):
        """
        Инициализирует веб-сервер для потоковой передачи аудио.
        
        Args:
            playlist_manager (PlaylistManager): Менеджер плейлиста
        """
        self.logger = logging.getLogger('audio_streamer')
        self.playlist_manager = playlist_manager
        self.audio_streamer = AudioStreamer()
        self.app = Flask(__name__)
        self.current_tracks = {}  # Словарь {route: current_track}
        
        # Настройки для таймаутов и потоков
        self.stream_chunk_size = 4096  # Размер чанка для стриминга
        self.client_timeout = 60  # Таймаут клиента в секундах
        
        # Регистрация маршрутов и обработчиков ошибок
        self._register_routes()
        self._register_error_handlers()
    
    def _register_routes(self) -> None:
        """
        Регистрирует маршруты Flask для веб-сервера.
        """
        try:
            # Получаем доступные маршруты из менеджера плейлистов
            available_routes = self.playlist_manager.get_available_routes()
            self.logger.info(f"Доступные маршруты для аудиопотоков: {', '.join(available_routes)}")
            
            # Динамическая регистрация маршрутов для аудиопотока
            for route in available_routes:
                self._register_stream_route(route)
            
            @self.app.route('/web')
            def web_interface():
                """Веб-интерфейс для браузеров (доступен только по специальному URL)."""
                return render_template_string(HTML_TEMPLATE)
            
            @self.app.route('/stream')
            def stream():
                """Эндпоинт для потоковой передачи аудио через веб-интерфейс."""
                return Response(
                    stream_with_context(self._generate_audio_stream()),
                    mimetype='audio/mpeg'
                )
                
            @self.app.route('/direct-stream')
            def direct_stream():
                """
                Прямой аудиопоток для радиоприемников (legacy URL).
                """
                return self._create_direct_stream_response()
                
            @self.app.route('/now-playing')
            def now_playing():
                """Эндпоинт для получения информации о текущем треке."""
                # Получаем маршрут, для которого нужна информация о треке
                route = request.args.get('route', None)
                track_name = "Нет активного трека"
                
                if route in self.current_tracks and self.current_tracks[route]:
                    track_name = os.path.basename(self.current_tracks[route])
                elif '/' in self.current_tracks and self.current_tracks['/']:
                    # Если маршрут не указан или для него нет трека, возвращаем информацию о треке для маршрута по умолчанию
                    track_name = os.path.basename(self.current_tracks['/'])
                
                return jsonify({"track": track_name, "route": route if route else "/"})
            
            @self.app.route('/reload-playlist')
            def reload_playlist():
                """Эндпоинт для перезагрузки плейлиста."""
                try:
                    route = request.args.get('route', None)
                    self.playlist_manager.reload_playlist(route)
                    if route:
                        return jsonify({"status": "success", "message": f"Плейлист для маршрута {route} перезагружен"})
                    return jsonify({"status": "success", "message": "Все плейлисты перезагружены"})
                except Exception as e:
                    self.logger.error(f"Ошибка при перезагрузке плейлиста: {e}", exc_info=True)
                    return jsonify({"status": "error", "message": str(e)})
            
            # Добавляем маршрут для списка доступных аудиопотоков
            @self.app.route('/streams')
            def list_streams():
                """Эндпоинт для получения списка доступных аудиопотоков."""
                streams = []
                for route in self.playlist_manager.get_available_routes():
                    stream_info = {
                        "route": route,
                        "url": request.host_url.rstrip('/') + route,
                    }
                    # Добавляем информацию о текущем треке, если есть
                    if route in self.current_tracks and self.current_tracks[route]:
                        stream_info["current_track"] = os.path.basename(self.current_tracks[route])
                    
                    streams.append(stream_info)
                
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
        
        # Создаем замыкание для передачи route в функцию
        def create_stream_handler(current_route):
            def stream_handler():
                self.logger.info(f"Запрос на аудиопоток через маршрут: {current_route}")
                return self._create_direct_stream_response(current_route)
            return stream_handler
        
        # Создаем обработчик с замыканием
        stream_handler = create_stream_handler(route)
        
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
            
            @self.app.errorhandler(500)
            def handle_server_error(e):
                """
                Обработчик внутренних ошибок сервера.
                """
                self.logger.error(f"Внутренняя ошибка сервера: {str(e)}")
                return Response("Внутренняя ошибка сервера. Пожалуйста, повторите попытку позже.", 
                                status=500, 
                                mimetype='text/plain')
            
            @self.app.errorhandler(Exception)
            def handle_unhandled_exception(e):
                """
                Обработчик необработанных исключений.
                """
                self.logger.error(f"Необработанное исключение: {str(e)}", exc_info=True)
                return Response("Произошла непредвиденная ошибка. Пожалуйста, повторите попытку позже.", 
                                status=500, 
                                mimetype='text/plain')
        
        except Exception as e:
            self.logger.error(f"Ошибка при регистрации обработчиков ошибок: {e}", exc_info=True)
    
    def _create_direct_stream_response(self, route: str = '/'):
        """
        Создает ответ для прямого аудиопотока
        
        Args:
            route (str): URL-путь, для которого создается поток
            
        Returns:
            Response: Flask-ответ с аудиопотоком и необходимыми заголовками
        """
        return Response(
            stream_with_context(self._generate_audio_stream(route)),
            mimetype='audio/mpeg',
            headers={
                # Заголовки для совместимости с радиоприемниками и автоматического воспроизведения
                'Content-Type': 'audio/mpeg',
                'icy-name': f'Audio Stream Server - {route}',
                'icy-description': f'Direct audio stream for {route}',
                'icy-genre': 'Various',
                'icy-br': '192',
                'Cache-Control': 'no-cache, no-store, must-revalidate',
                'Pragma': 'no-cache',
                'Expires': '0',
                'X-Content-Type-Options': 'nosniff',
                'Connection': 'keep-alive'  # Явно указываем keep-alive для стриминга
            }
        )
    
    def _generate_audio_stream(self, route: str = '/') -> Generator[bytes, None, None]:
        """
        Генератор для потоковой передачи аудио для указанного маршрута.
        
        Args:
            route (str): URL-путь, для которого генерируется поток
            
        Yields:
            bytes: Чанки аудиоданных
        """
        # Создаем объект для обработки разрыва соединения
        disconnect_event = threading.Event()
        audio_stream = None
        
        try:
            # Получаем информацию о клиенте для логирования
            client_address = request.remote_addr if request else "unknown"
            self.logger.info(f"Начало аудиопотока для клиента {client_address} на маршруте {route}")
            
            while not disconnect_event.is_set():
                track = self.playlist_manager.get_next_track(route)
                if not track:
                    # Если нет треков, ждем и пробуем снова
                    self.logger.warning(f"Нет доступных треков для воспроизведения на маршруте {route}, ожидание...")
                    # Возвращаем небольшую паузу вместо бесконечного ожидания
                    yield b"\x00" * self.stream_chunk_size
                    time.sleep(2)
                    continue
                
                # Сохраняем текущий трек для этого маршрута
                self.current_tracks[route] = track
                
                file_name = os.path.basename(track)
                self.logger.info(f"Начало трансляции трека на маршруте {route}: {file_name}")
                
                try:
                    # Создаем поток аудио для текущего трека
                    audio_stream, _ = self.audio_streamer.create_stream_from_file(track)
                    if not audio_stream:
                        self.logger.error(f"Не удалось создать аудиопоток для файла: {file_name}")
                        continue
                    
                    # Устанавливаем таймаут для сокета, если это возможно
                    if hasattr(audio_stream, 'timeout'):
                        audio_stream.timeout = self.client_timeout
                    
                    # Потоковая передача данных
                    while not disconnect_event.is_set():
                        try:
                            # Читаем данные с таймаутом
                            chunk = audio_stream.read(self.stream_chunk_size)
                            if not chunk:
                                # Конец файла, переходим к следующему треку
                                break
                            
                            # Возвращаем чанк данных
                            yield chunk
                            
                            # Небольшая пауза для предотвращения перегрузки CPU
                            time.sleep(0.001)
                            
                        except socket.timeout:
                            self.logger.warning(f"Таймаут при чтении аудиоданных для клиента {client_address}")
                            # Отправляем пустой блок и продолжаем
                            yield b""
                            continue
                        except (IOError, ConnectionError, BrokenPipeError) as e:
                            self.logger.warning(f"Соединение с клиентом {client_address} прервано: {str(e)}")
                            disconnect_event.set()
                            break
                        except Exception as e:
                            self.logger.error(f"Ошибка при чтении аудиопотока: {e}", exc_info=True)
                            break
                
                except Exception as e:
                    self.logger.error(f"Ошибка при обработке аудиопотока для трека {file_name}: {e}", exc_info=True)
                
                finally:
                    # Закрываем поток
                    if audio_stream:
                        try:
                            audio_stream.close()
                            self.logger.debug(f"Аудиопоток для трека {file_name} закрыт")
                        except Exception as e:
                            self.logger.warning(f"Ошибка при закрытии аудиопотока: {e}")
        
        except (ConnectionError, BrokenPipeError, IOError) as e:
            self.logger.warning(f"Клиент {client_address} отключился: {str(e)}")
        except Exception as e:
            self.logger.error(f"Ошибка при генерации аудиопотока для маршрута {route}: {e}", exc_info=True)
        
        finally:
            # Закрываем поток в случае любой ошибки
            if audio_stream:
                try:
                    audio_stream.close()
                except:
                    pass
            
            self.logger.info(f"Завершение аудиопотока для клиента {client_address} на маршруте {route}")
    
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

# Если запускается непосредственно этот файл, создаем и запускаем WSGI-приложение
def create_app(playlist_path=None, directory_routes=None):
    """
    Создает Flask приложение для использования с WSGI-серверами
    
    Args:
        playlist_path (str, optional): Путь к файлу плейлиста или директории по умолчанию
        directory_routes (Dict[str, str], optional): Словарь маршрутов и соответствующих им директорий
        
    Returns:
        Flask: Flask-приложение
    """
    from playlist_manager import PlaylistManager
    
    # Создаем менеджер плейлистов
    playlist_manager = PlaylistManager(playlist_path, directory_routes)
    server = AudioStreamServer(playlist_manager)
    return server.app

# Эта функция позволит использовать WSGI-серверы вроде Gunicorn
application = create_app() 