package announce

import (
	"testing"
	"time"
)

func TestNumbersInWords(t *testing.T) {
	cases := []struct {
		n        int
		feminine bool
		want     string
	}{
		{1, true, "одна"}, {2, true, "две"}, {1, false, "один"}, {12, true, "двенадцать"},
		{21, true, "двадцать одна"}, {40, true, "сорок"}, {75, false, "семьдесят пять"},
		{2019, false, "две тысячи девятнадцать"}, {2023, false, "две тысячи двадцать три"},
		{1000, false, "тысяча"}, {5001, true, "пять тысяч одна"},
	}
	for _, c := range cases {
		if got := Cardinal(c.n, c.feminine); got != c.want {
			t.Errorf("Cardinal(%d, %v) = %q, want %q", c.n, c.feminine, got, c.want)
		}
	}
	for n, want := range map[int]string{1: "первая", 3: "третья", 11: "одиннадцатая", 20: "двадцатая",
		32: "тридцать вторая", 34: "тридцать четвёртая", 40: "сороковая"} {
		if got := OrdinalFem(n); got != want {
			t.Errorf("OrdinalFem(%d) = %q, want %q", n, got, want)
		}
	}
	for n, want := range map[int]string{1: "минута", 2: "минуты", 5: "минут", 11: "минут", 21: "минута", 24: "минуты", 112: "минут"} {
		if got := Plural(n, "минута", "минуты", "минут"); got != want {
			t.Errorf("Plural(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestSpokenTitle(t *testing.T) {
	cases := map[string]string{
		// Lectures, renamed by the family.
		"2019 Пси-заклинания 5.mp3":                          "две тысячи девятнадцать, Пси-заклинания пять",
		"Технология работы с инопланетной монадой - 2.1.mp3": "Технология работы с инопланетной монадой, два один",
		"Обретение Высшего Учителя - 5-2.mp3":                "Обретение Высшего Учителя, пять два",
		"Фиксация на Уровне 4.1 - 01.mp3":                    "Фиксация на Уровне четыре один, один",
		"01 Книга 1, Глава первая.mp3":                       "Книга один, Глава первая",
		"16 Лидерский - 8.mp3":                               "Лидерский, восемь",
		"3.mp3":                                              "Часть третья",
		"P.S. Животные-Артефакты.mp3":                        "постскриптум Животные-Артефакты",
		"Психокинетика как система развития в модели Атлантов 02-1.mp3": "Психокинетика как система развития в модели Атлантов два один",
		// Songs in translit.
		"1_01. Perekati-Pole.mp3":                "Перекати-Поле",
		"4_11. Chego Ty Zhdyosh'.mp3":            "Чего Ты Ждёшь",
		"2_11. Pirozhki S Kartoshechkoi'.mp3":    "Пирожки С Картошечкой",
		"10_5-Cyganskaja-pesnja.mp3":             "Цыганская-песня",
		"Vladimir_Troshin-Berezy-kissvk.com.mp3": "Владимир Трошин-Березы",
		// Downloaded videos.
		"I Gryanul Grem - ГУДКОВ： ＂Все плохо, но появилась надежда＂. Кремль готовит эскалацию, ПЕРЕГОВОРЫ, выборы в США, Трамп.mp3": "И грянул Грэм, Гудков: Все плохо, но появилась надежда",
		"The Breakfast Show - Деньги просто отнимут ｜ Игорь Липсиц на Breakfast Show.mp3":                                           "Брекфаст шоу, Деньги просто отнимут. Игорь Липсиц на Брекфаст шоу",
		"ВОТ ТАК - ‼️ Атакован один из крупнейших НПЗ России. Десятки погибших в Запорожской области ⧸ Выпуск новостей.mp3":         "ВОТ ТАК, Атакован один из крупнейших НПЗ России",
	}
	for name, want := range cases {
		if got := SpokenTitle(name); got != want {
			t.Errorf("SpokenTitle(%q)\n got  %q\n want %q", name, got, want)
		}
	}
}

func TestTimePhrase(t *testing.T) {
	m := time.Minute
	cases := []struct {
		elapsed, total time.Duration
		want           string
	}{
		{12*m + 30*time.Second, 40 * m, "Прошло двенадцать минут, осталось двадцать восемь минут."},
		{21 * m, 43 * m, "Прошла двадцать одна минута, осталось двадцать две минуты."},
		{5 * m, 6*m + 10*time.Second, "Прошло пять минут, осталось две минуты."},
		{10 * m, 11 * m, "Прошло десять минут, осталась одна минута."},
		{20 * time.Second, 40 * m, "Запись только началась, она длится сорок минут."},
		{0, 21 * m, "Запись только началась, она длится двадцать одну минуту."},
		{40 * m, 40 * m, "Запись заканчивается."},
		{5 * m, 0, ""},
	}
	for _, c := range cases {
		if got := TimePhrase(c.elapsed, c.total); got != c.want {
			t.Errorf("TimePhrase(%v, %v) = %q, want %q", c.elapsed, c.total, got, c.want)
		}
	}
	if got := NextRecord("2023 Работа в высокочастотном режиме 2.mp3"); got != "Дальше: две тысячи двадцать три, Работа в высокочастотном режиме два." {
		t.Errorf("NextRecord = %q", got)
	}
	if got := StationIntro(32, "Целительство"); got != "Тридцать вторая станция. Целительство." {
		t.Errorf("StationIntro = %q", got)
	}
}
