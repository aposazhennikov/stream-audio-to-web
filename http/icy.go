package http

import (
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// icyMetaInt — через сколько байт звука вставляется блок метаданных (как у Icecast/SHOUTcast).
	icyMetaInt = 16000
	// Длина блока задаётся одним байтом в единицах по 16 байт.
	icyMaxMetadataLen = 255 * 16
)

// icyMetadataWriter inserts ICY (SHOUTcast) metadata blocks into an MP3 stream so that
// players like VLC, phone apps and car radios show the current track title.
// Titles are sent in UTF-8: VLC, Android and iOS players display Russian titles correctly.
type icyMetadataWriter struct {
	w         io.Writer
	untilMeta int
	title     func() string
	lastTitle string
	sentOnce  bool
}

func newICYMetadataWriter(w io.Writer, title func() string) *icyMetadataWriter {
	return &icyMetadataWriter{w: w, untilMeta: icyMetaInt, title: title}
}

// Write writes audio and inserts a metadata block every icyMetaInt bytes of audio.
func (iw *icyMetadataWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > iw.untilMeta {
			n = iw.untilMeta
		}
		m, err := iw.w.Write(p[:n])
		written += m
		if err != nil {
			return written, err
		}
		p = p[n:]
		iw.untilMeta -= n

		if iw.untilMeta == 0 {
			if _, err := iw.w.Write(iw.nextMetadataBlock()); err != nil {
				return written, err
			}
			iw.untilMeta = icyMetaInt
		}
	}
	return written, nil
}

// nextMetadataBlock returns the block for the current title, or a single zero byte
// ("no change") when the title is the same as in the previous block.
func (iw *icyMetadataWriter) nextMetadataBlock() []byte {
	title := iw.title()
	if iw.sentOnce && title == iw.lastTitle {
		return []byte{0}
	}
	iw.lastTitle = title
	iw.sentOnce = true
	return buildICYMetadataBlock(title)
}

// buildICYMetadataBlock builds "StreamTitle='...';" padded to a multiple of 16 bytes,
// prefixed with its length in 16-byte units.
func buildICYMetadataBlock(title string) []byte {
	const prefix, suffix = "StreamTitle='", "';"
	// Апостроф внутри названия обрывает значение у большинства плееров; заменяем на типографский.
	title = strings.ReplaceAll(title, "'", "’")
	title = strings.Map(func(r rune) rune {
		if r < ' ' {
			return ' '
		}
		return r
	}, title)
	title = truncateUTF8(title, icyMaxMetadataLen-len(prefix)-len(suffix))

	meta := prefix + title + suffix
	blocks := (len(meta) + 15) / 16
	out := make([]byte, 1+blocks*16)
	out[0] = byte(blocks)
	copy(out[1:], meta)
	return out
}

// truncateUTF8 cuts s to at most maxBytes without splitting a multi-byte character.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// trackTitleFromPath turns a file path into a title: file name without extension.
func trackTitleFromPath(trackPath string) string {
	if trackPath == "" {
		return ""
	}
	name := filepath.Base(trackPath)
	return strings.TrimSuffix(name, filepath.Ext(name))
}
