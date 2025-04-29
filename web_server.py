#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
Модуль веб-сервера для потоковой передачи аудио.
"""

import os
import logging
import threading
import time
from typing import Generator, Optional
from flask import Flask, Response, render_template_string, stream_with_context, jsonify
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
        .controls {
            margin: 15px 0;
        }
        button {
            background-color: #3498db;
            color: white;
            border: none;
            padding: 8px 15px;
            border-radius: 4px;
            cursor: pointer;
            margin: 0 5px;
        }
        button:hover {
            background-color: #2980b9;
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
        
        // Ручное управление аудиоплеером для надежного воспроизведения
        function setupAudioPlayer() {
            const audioPlayer = document.getElementById('audio-player');
            
            // Автоматический переход к следующему треку в случае ошибки
            audioPlayer.addEventListener('error', () => {
                console.log('Ошибка воспроизведения, обновление источника...');
                audioPlayer.src = '/stream?nocache=' + new Date().getTime();
                audioPlayer.load();
                audioPlayer.play().catch(e => console.error('Не удалось воспроизвести после ошибки:', e));
            });
            
            // Кнопки управления
            document.getElementById('play-btn').addEventListener('click', () => {
                audioPlayer.play().catch(e => console.error('Не удалось воспроизвести:', e));
            });
            
            document.getElementById('stop-btn').addEventListener('click', () => {
                audioPlayer.pause();
                audioPlayer.currentTime = 0;
            });
            
            document.getElementById('reload-btn').addEventListener('click', () => {
                audioPlayer.src = '/stream?nocache=' + new Date().getTime();
                audioPlayer.load();
                audioPlayer.play().catch(e => console.error('Не удалось перезагрузить:', e));
                updateCurrentTrack();
            });
        }
        
        window.onload = function() {
            // Первоначальное обновление информации о треке
            updateCurrentTrack();
            setupAudioPlayer();
            
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
            
            <div class="controls">
                <button id="play-btn">▶ Воспроизвести</button>
                <button id="stop-btn">⏹ Остановить</button>
                <button id="reload-btn">🔄 Перезагрузить</button>
            </div>
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
        current_track (str): Текущий проигрываемый трек
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
        self.current_track = None
        
        # Регистрация маршрутов
        self._register_routes()
    
    def _register_routes(self) -> None:
        """
        Регистрирует маршруты Flask для веб-сервера.
        """
        try:
            @self.app.route('/')
            def index():
                """Главная страница с аудиоплеером."""
                return render_template_string(HTML_TEMPLATE)
            
            @self.app.route('/stream')
            def stream():
                """Эндпоинт для потоковой передачи аудио."""
                return Response(
                    stream_with_context(self._generate_audio_stream()),
                    mimetype='audio/mpeg'
                )
                
            @self.app.route('/now-playing')
            def now_playing():
                """Эндпоинт для получения информации о текущем треке."""
                track_name = "Нет активного трека"
                if self.current_track:
                    track_name = os.path.basename(self.current_track)
                return jsonify({"track": track_name})
            
            @self.app.route('/reload-playlist')
            def reload_playlist():
                """Эндпоинт для перезагрузки плейлиста."""
                try:
                    self.playlist_manager.reload_playlist()
                    return jsonify({"status": "success", "message": "Плейлист перезагружен"})
                except Exception as e:
                    self.logger.error(f"Ошибка при перезагрузке плейлиста: {e}", exc_info=True)
                    return jsonify({"status": "error", "message": str(e)})
        
        except Exception as e:
            self.logger.error(f"Ошибка при регистрации маршрутов: {e}", exc_info=True)
    
    def _generate_audio_stream(self) -> Generator[bytes, None, None]:
        """
        Генератор для потоковой передачи аудио.
        
        Yields:
            bytes: Чанки аудиоданных
        """
        try:
            while True:
                track = self._get_next_track()
                if not track:
                    # Если нет треков, ждем и пробуем снова
                    self.logger.warning("Нет доступных треков для воспроизведения, ожидание...")
                    time.sleep(2)
                    continue
                
                self.current_track = track
                file_name = os.path.basename(track)
                self.logger.info(f"Начало трансляции трека: {file_name}")
                
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
            self.logger.error(f"Ошибка при генерации аудиопотока: {e}", exc_info=True)
            # Возвращаем пустые данные, чтобы не прерывать поток
            yield b''
    
    def _get_next_track(self) -> Optional[str]:
        """
        Получает следующий трек из плейлиста.
        
        Returns:
            Optional[str]: Путь к следующему треку или None, если плейлист пуст
        """
        try:
            return self.playlist_manager.get_next_track()
        except Exception as e:
            self.logger.error(f"Ошибка при получении следующего трека: {e}", exc_info=True)
            return None
    
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
            self.app.run(host=host, port=port, debug=debug, threaded=True)
        except Exception as e:
            self.logger.error(f"Ошибка при запуске аудио-сервера: {e}", exc_info=True) 