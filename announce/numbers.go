package announce

import "strings"

// Russian numbers in words. The synthesizer reads digits without agreement ("один минута"),
// so every number that is spoken is written out here.

var (
	unitsMasc = []string{"ноль", "один", "два", "три", "четыре", "пять", "шесть", "семь", "восемь", "девять"}
	unitsFem  = []string{"ноль", "одна", "две", "три", "четыре", "пять", "шесть", "семь", "восемь", "девять"}
	teens     = []string{"десять", "одиннадцать", "двенадцать", "тринадцать", "четырнадцать", "пятнадцать",
		"шестнадцать", "семнадцать", "восемнадцать", "девятнадцать"}
	tens = []string{"", "", "двадцать", "тридцать", "сорок", "пятьдесят", "шестьдесят", "семьдесят",
		"восемьдесят", "девяносто"}
	hundreds = []string{"", "сто", "двести", "триста", "четыреста", "пятьсот", "шестьсот", "семьсот",
		"восемьсот", "девятьсот"}

	// Ordinal, feminine, nominative ("тридцать вторая станция").
	ordUnitsFem = []string{"", "первая", "вторая", "третья", "четвёртая", "пятая", "шестая", "седьмая",
		"восьмая", "девятая"}
	ordTeensFem = []string{"десятая", "одиннадцатая", "двенадцатая", "тринадцатая", "четырнадцатая",
		"пятнадцатая", "шестнадцатая", "семнадцатая", "восемнадцатая", "девятнадцатая"}
	ordTensFem = []string{"", "", "двадцатая", "тридцатая", "сороковая", "пятидесятая", "шестидесятая",
		"семидесятая", "восьмидесятая", "девяностая"}
)

// Cardinal writes 0..9999 in words; feminine for words like "минута".
func Cardinal(n int, feminine bool) string {
	if n < 0 || n > 9999 {
		return ""
	}
	if n == 0 {
		return "ноль"
	}
	var words []string
	if th := n / 1000; th > 0 {
		switch {
		case th == 1:
			words = append(words, "тысяча")
		case th == 2:
			words = append(words, "две", "тысячи")
		case th <= 4:
			words = append(words, unitsMasc[th], "тысячи")
		default:
			words = append(words, unitsMasc[th], "тысяч")
		}
		n %= 1000
	}
	if h := n / 100; h > 0 {
		words = append(words, hundreds[h])
		n %= 100
	}
	switch {
	case n >= 10 && n < 20:
		words = append(words, teens[n-10])
	default:
		if t := n / 10; t > 0 {
			words = append(words, tens[t])
		}
		if u := n % 10; u > 0 {
			if feminine {
				words = append(words, unitsFem[u])
			} else {
				words = append(words, unitsMasc[u])
			}
		}
	}
	return strings.Join(words, " ")
}

// OrdinalFem writes 1..99 as a feminine ordinal ("тридцать вторая").
func OrdinalFem(n int) string {
	switch {
	case n <= 0 || n > 99:
		return ""
	case n < 10:
		return ordUnitsFem[n]
	case n < 20:
		return ordTeensFem[n-10]
	case n%10 == 0:
		return ordTensFem[n/10]
	default:
		return tens[n/10] + " " + ordUnitsFem[n%10]
	}
}

// Plural picks the noun form for n: one ("минута"), few ("минуты"), many ("минут").
func Plural(n int, one, few, many string) string {
	n %= 100
	switch {
	case n >= 11 && n <= 14:
		return many
	case n%10 == 1:
		return one
	case n%10 >= 2 && n%10 <= 4:
		return few
	default:
		return many
	}
}
