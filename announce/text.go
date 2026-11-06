package announce

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Readings of Latin names that occur in file names (channels of the downloaded videos and
// the like). Matched case-insensitively as whole phrases; longer phrases first.
var latinReadings = [][2]string{
	{"The Breakfast Show", "Брекфаст шоу"},
	{"Breakfast Show", "Брекфаст шоу"},
	{"I Gryanul Grem", "И грянул Грэм"},
	{"Aleksandr Nevzorov", "Александр Невзоров"},
	{"Alexey Arestovych", "Алексей Арестович"},
	{"Sergey Aleksashenko", "Сергей Алексашенко"},
	{"Vladimir Troshin", "Владимир Трошин"},
	{"ScienceVideoLab", "Сайнс видео лаб"},
	{"Vert Dider", "Верт Дидер"},
	{"Sheinkin40", "Шейнкин сорок"},
	{"Sheinkin", "Шейнкин"},
	{"Redroom", "Редрум"},
	{"QWERTY", "Кверти"},
	{"SVTV", "Эс Вэ Тэ Вэ"},
	{"OpenAI", "Опен эй ай"},
	{"Anthropic", "Антропик"},
	{"AGI", "эй джи ай"},
	{"CD", "си ди"},
	{"G20", "Джи двадцать"},
	{"Ozon", "Озон"},
	{"Live", "лайв"},
	{"News", "ньюс"},
	{"P.S.", "постскриптум"},
}

var (
	reOrderPrefix = regexp.MustCompile(`^\d{1,3}(?:_\d{1,3})?\s*[-–—.]?\s*`)
	reYearFirst   = regexp.MustCompile(`^((?:19|20)\d\d)\s+`)
	rePartPair    = regexp.MustCompile(`(\d+)[.-](\d+)`)
	reNumber      = regexp.MustCompile(`\d+`)
	reSpaces      = regexp.MustCompile(`\s+`)
	reLatinWord   = regexp.MustCompile(`[A-Za-z][A-Za-z']*`)
	reJunk        = regexp.MustCompile(`(?i)[-_ ]*kissvk\.com|\s*-\s*аудиокнига|@\S+|#\S+`)
)

// SpokenTitle turns an audio file name into a short phrase for the synthesizer: no extension,
// no order prefix ("01 ", "1_01. "), no emoji or channel junk, numbers in words ("2.1" is read
// as "два один"), Latin names in Russian reading, long titles cut to the first sentence.
func SpokenTitle(fileName string) string {
	s := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	s = halfWidth(s)
	s = reJunk.ReplaceAllString(s, "")
	s = dropSymbols(s)

	if rest := reOrderPrefix.ReplaceAllString(s, ""); !reYearFirst.MatchString(s) && hasLetter(rest) {
		s = rest
	}
	s = strings.ReplaceAll(s, "_", " ")
	if !hasLetter(s) {
		// The whole name is a number: a numbered part of the station's course.
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 && n < 100 {
			return "Часть " + OrdinalFem(n)
		}
	}

	s = reYearFirst.ReplaceAllString(s, "$1, ")
	s = cutLongTitle(s)
	s = rePartPair.ReplaceAllString(s, "$1 $2")
	s = reNumber.ReplaceAllStringFunc(s, func(d string) string {
		n, err := strconv.Atoi(d)
		if err != nil || n > 9999 {
			return d
		}
		return Cardinal(n, false)
	})
	s = readLatin(s)
	s = softenCaps(s)
	s = strings.ReplaceAll(s, " - ", ", ")
	s = strings.ReplaceAll(s, " — ", ", ")
	s = strings.ReplaceAll(s, " | ", ". ")
	s = strings.ReplaceAll(s, " / ", ". ")
	s = strings.Trim(reSpaces.ReplaceAllString(s, " "), " ,.-")
	return s
}

// halfWidth maps full-width punctuation (common in downloaded video titles) to ASCII.
func halfWidth(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			return r - 0xFEE0
		case r == '⧸':
			return '/'
		}
		return r
	}, s)
}

// dropSymbols removes emoji and other symbols; the synthesizer would read or garble them.
// The separators "|" and "/" stay: they end a sentence of a video title.
func dropSymbols(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '|' || r == '/' || r == '—' || r == '–':
			return r
		case r == '*' || r == '"' || r == '«' || r == '»' || r == '#':
			return -1
		case unicode.IsSymbol(r) || unicode.Is(unicode.Variation_Selector, r) || r == 0x200d:
			return -1
		case unicode.IsMark(r):
			return -1
		case r >= 0x2000 && !unicode.IsLetter(r) && !unicode.IsSpace(r) && !strings.ContainsRune("…’‘“”", r):
			return -1 // emoji-like punctuation such as ‼ and ⁉
		}
		return r
	}, s)
}

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// cutLongTitle keeps the channel and the first sentence of a long video title.
func cutLongTitle(s string) string {
	const maxWords = 14
	if len(strings.Fields(s)) <= maxWords {
		return s
	}
	channel, title := "", s
	if i := strings.Index(s, " - "); i > 0 && i < 40 {
		channel, title = s[:i], s[i+3:]
	}
	if j := strings.IndexAny(title, ".|/?!"); j > 0 {
		title = title[:j+1]
		title = strings.TrimRight(title, "./| ")
	}
	words := strings.Fields(title)
	if len(words) > maxWords {
		words = words[:maxWords]
	}
	title = strings.Join(words, " ")
	if channel != "" {
		return channel + " - " + title
	}
	return title
}

// readLatin replaces known Latin names with their Russian reading and transliterates the
// remaining Latin words (song titles are written in translit: "Perekati-Pole").
func readLatin(s string) string {
	lower := strings.ToLower(s)
	for _, pair := range latinReadings {
		key := strings.ToLower(pair[0])
		for {
			i := strings.Index(lower, key)
			if i < 0 {
				break
			}
			s = s[:i] + pair[1] + s[i+len(key):]
			lower = strings.ToLower(s)
		}
	}
	return reLatinWord.ReplaceAllStringFunc(s, translit)
}

var translitPairs = []struct{ lat, cyr string }{
	{"shch", "щ"}, {"sch", "щ"}, {"zh", "ж"}, {"kh", "х"}, {"ts", "ц"}, {"ch", "ч"}, {"sh", "ш"},
	{"yo", "ё"}, {"jo", "ё"}, {"yu", "ю"}, {"ju", "ю"}, {"ya", "я"}, {"ja", "я"}, {"ye", "е"},
	{"i'", "й"}, {"cy", "цы"}, {"ci", "ци"}, {"ce", "це"},
	{"a", "а"}, {"b", "б"}, {"v", "в"}, {"g", "г"}, {"d", "д"}, {"e", "е"}, {"z", "з"}, {"i", "и"},
	{"j", "й"}, {"k", "к"}, {"l", "л"}, {"m", "м"}, {"n", "н"}, {"o", "о"}, {"p", "п"}, {"r", "р"},
	{"s", "с"}, {"t", "т"}, {"u", "у"}, {"f", "ф"}, {"h", "х"}, {"c", "к"}, {"y", "ы"}, {"w", "в"},
	{"x", "кс"}, {"q", "к"}, {"'", "ь"},
}

// translit reads a Latin word as Russian translit ("Zhdyosh'" -> "Ждёшь").
func translit(word string) string {
	lower := strings.ToLower(word)
	var b strings.Builder
	for i := 0; i < len(lower); {
		matched := false
		for _, p := range translitPairs {
			if strings.HasPrefix(lower[i:], p.lat) {
				cyr := p.cyr
				// "y" after a vowel is "й" ("Proi'det", "Moei'"), at the word start before a vowel too.
				if p.lat == "y" && (i == 0 || strings.ContainsRune("aeiou", rune(lower[i-1]))) {
					cyr = "й"
				}
				b.WriteString(cyr)
				i += len(p.lat)
				matched = true
				break
			}
		}
		if !matched {
			i++
		}
	}
	out := []rune(b.String())
	if len(out) > 0 && unicode.IsUpper(rune(word[0])) {
		out[0] = unicode.ToUpper(out[0])
	}
	return string(out)
}

// softenCaps lowers long words written in capitals ("ГУДКОВ", "ЧТО ДАЛЬШЕ"); short ones
// (США, НПЗ) stay, the synthesizer spells them.
func softenCaps(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		letters, upper := 0, 0
		for _, r := range w {
			if unicode.IsLetter(r) {
				letters++
				if unicode.IsUpper(r) {
					upper++
				}
			}
		}
		if letters >= 5 && upper == letters {
			r := []rune(strings.ToLower(w))
			r[0] = unicode.ToUpper(r[0])
			words[i] = string(r)
		}
	}
	return strings.Join(words, " ")
}

// StationIntro: "Тридцать вторая станция. Целительство."
func StationIntro(preset int, name string) string {
	number := OrdinalFem(preset)
	if number == "" {
		return name + "."
	}
	r := []rune(number)
	r[0] = unicode.ToUpper(r[0])
	return fmt.Sprintf("%s станция. %s.", string(r), name)
}

// NowPlaying: "Сейчас звучит: <title>."
func NowPlaying(fileName string) string {
	title := SpokenTitle(fileName)
	if title == "" {
		return ""
	}
	return "Сейчас звучит: " + title + "."
}

// NextRecord is said when the station moves on to the next record while the listener stays:
// "Дальше: <title>."
func NextRecord(fileName string) string {
	title := SpokenTitle(fileName)
	if title == "" {
		return ""
	}
	return "Дальше: " + title + "."
}

// TimePhrase tells how much of the record has passed and how much is left:
// "Прошло двенадцать минут, осталось двадцать восемь минут."
func TimePhrase(elapsed, total time.Duration) string {
	if total <= 0 {
		return ""
	}
	if elapsed < 0 {
		elapsed = 0
	}
	passed := int(elapsed / time.Minute)
	left := int((total - elapsed + time.Minute - 1) / time.Minute)
	if left < 0 {
		left = 0
	}
	totalMin := int((total + time.Minute/2) / time.Minute)
	minutes := func(n int) string {
		return Cardinal(n, true) + " " + Plural(n, "минута", "минуты", "минут")
	}
	minutesAcc := func(n int) string {
		words := Cardinal(n, true)
		if strings.HasSuffix(words, "одна") { // accusative: "длится двадцать одну минуту"
			words = strings.TrimSuffix(words, "одна") + "одну"
		}
		return words + " " + Plural(n, "минуту", "минуты", "минут")
	}
	switch {
	case passed == 0:
		if totalMin < 1 {
			return "Запись только началась."
		}
		return "Запись только началась, она длится " + minutesAcc(totalMin) + "."
	case left == 0:
		return "Запись заканчивается."
	}
	passedVerb := "Прошло"
	if Plural(passed, "1", "2", "5") == "1" {
		passedVerb = "Прошла"
	}
	leftVerb := "осталось"
	if Plural(left, "1", "2", "5") == "1" {
		leftVerb = "осталась"
	}
	return fmt.Sprintf("%s %s, %s %s.", passedVerb, minutes(passed), leftVerb, minutes(left))
}

// ButtonPhrase is said when the listener arrives on a button.
func ButtonPhrase(next bool, stationName string) string {
	if next {
		return "Кнопка вперёд. " + stationName + ". Нажмите ещё раз — следующая запись."
	}
	return "Кнопка назад. " + stationName + ". Нажмите ещё раз — предыдущая запись."
}
