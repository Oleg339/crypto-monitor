// quick_parse — быстрая проверка парсинга одного сообщения
package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type ParsedSignal struct {
	ID        int
	Symbol    string
	Direction string
	EntryLow  float64
	EntryHigh float64
	SL        float64
	TPs       []float64
	Raw       string
}

var (
	reSignalID  = regexp.MustCompile(`(?i)SIGNAL\s*ID\s*:\s*#(\d+)`)
	reCoin      = regexp.MustCompile(`(?i)COIN\s*:\s*(\S+)`)
	reDirection = regexp.MustCompile(`(?i)Direction\s*:\s*(LONG|SHORT)`)
	reEntry     = regexp.MustCompile(`(?i)ENTRY\s*:\s*([^\n]+)`)
	reTargets   = regexp.MustCompile(`(?i)TARGETS\s*:\s*([^\n]+)`)
	reSLLine    = regexp.MustCompile(`(?i)STOP\s*LOSS\s*:\s*([^\n]+)`)
	reProfLoss  = regexp.MustCompile(`(?i)\d[\d.,]*\s*%\s*(PROFIT|LOSS)`)
)

func main() {
	fmt.Println("🎯 БЫСТРАЯ ПРОВЕРКА ПАРСЕРА")
	fmt.Println("=" + strings.Repeat("=", 30))
	fmt.Println("Вставьте сообщение от Трейдера 3 (завершите пустой строкой):")
	fmt.Println()

	// Читаем многострочное сообщение
	scanner := bufio.NewScanner(os.Stdin)
	var lines []string
	
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" && len(lines) > 0 {
			break
		}
		lines = append(lines, line)
	}
	
	if len(lines) == 0 {
		fmt.Println("❌ Сообщение не введено")
		return
	}
	
	text := strings.Join(lines, "\n")
	
	fmt.Println("\n" + strings.Repeat("-", 50))
	fmt.Printf("📝 Входное сообщение (%d символов):\n", len(text))
	fmt.Printf("```\n%s\n```\n", text)
	
	// Анализируем
	fmt.Println("\n🔍 АНАЛИЗ:")
	
	// Проверяем фильтры
	isValid, reason := checkSignalValid(text)
	if !isValid {
		fmt.Printf("❌ ОТФИЛЬТРОВАНО: %s\n", reason)
		return
	}
	
	fmt.Println("✅ Прошел все фильтры")
	
	// Показываем извлечение полей
	fmt.Println("\n📊 ИЗВЛЕЧЕННЫЕ ПОЛЯ:")
	showRegexMatches(text)
	
	// Парсим
	sig, ok := parseSignalText(text)
	if !ok {
		fmt.Println("\n❌ ОШИБКА ПАРСИНГА")
		return
	}
	
	// Показываем структуру
	fmt.Println("\n🎯 РЕЗУЛЬТИРУЮЩАЯ СТРУКТУРА:")
	printStructure(sig)
	
	// Показываем алерт
	fmt.Println("\n📱 TELEGRAM АЛЕРТ:")
	alert := formatSignalAlert(sig)
	fmt.Printf("```\n%s\n```\n", alert)
	
	fmt.Println("\n✅ ПАРСИНГ ЗАВЕРШЕН УСПЕШНО!")
}

func checkSignalValid(text string) (bool, string) {
	t := strings.ToUpper(text)
	
	if strings.Contains(t, "GEM SIGNAL") {
		return false, "содержит 'GEM SIGNAL'"
	}
	
	if reProfLoss.MatchString(text) {
		return false, "содержит информацию о прибыли/убытках"
	}
	
	if !reSignalID.MatchString(text) {
		return false, "отсутствует SIGNAL ID"
	}
	
	if !reEntry.MatchString(text) {
		return false, "отсутствует ENTRY"
	}
	
	if !reTargets.MatchString(text) {
		return false, "отсутствуют TARGETS"
	}
	
	if !reSLLine.MatchString(text) {
		return false, "отсутствует STOP LOSS"
	}
	
	return true, ""
}

func showRegexMatches(text string) {
	regexes := []struct {
		name string
		re   *regexp.Regexp
	}{
		{"Signal ID", reSignalID},
		{"Coin", reCoin},
		{"Direction", reDirection},
		{"Entry", reEntry},
		{"Targets", reTargets},
		{"Stop Loss", reSLLine},
	}
	
	for _, r := range regexes {
		match := r.re.FindStringSubmatch(text)
		if len(match) >= 2 {
			fmt.Printf("   ✅ %-10s: %q\n", r.name, strings.TrimSpace(match[1]))
		} else {
			fmt.Printf("   ❌ %-10s: не найден\n", r.name)
		}
	}
}

func printStructure(sig *ParsedSignal) {
	fmt.Printf("   📊 ParsedSignal {\n")
	fmt.Printf("       ID:        %d\n", sig.ID)
	fmt.Printf("       Symbol:    %q\n", sig.Symbol)
	fmt.Printf("       Direction: %q\n", sig.Direction)
	fmt.Printf("       EntryLow:  %.8g\n", sig.EntryLow)
	fmt.Printf("       EntryHigh: %.8g\n", sig.EntryHigh)
	fmt.Printf("       SL:        %.8g\n", sig.SL)
	fmt.Printf("       TPs:       %v\n", sig.TPs)
	fmt.Printf("   }\n")
	
	// Расчеты
	mid := (sig.EntryLow + sig.EntryHigh) / 2
	if mid > 0 {
		slPct := (mid - sig.SL) / mid * 100
		fmt.Printf("\n   💰 РАСЧЕТЫ:\n")
		fmt.Printf("       Entry Mid: %.8g\n", mid)
		fmt.Printf("       SL Risk:   -%.2f%%\n", slPct)
		
		if len(sig.TPs) > 0 {
			fmt.Printf("       TP Potential:\n")
			bestRR := 0.0
			for i, tp := range sig.TPs {
				tpPct := (tp - mid) / mid * 100
				rr := tpPct / slPct
				if rr > bestRR {
					bestRR = rr
				}
				fmt.Printf("         TP%d: %.8g (+%.2f%%, R:R %.2f:1)\n", i+1, tp, tpPct, rr)
			}
			fmt.Printf("       Best R:R:  %.2f:1\n", bestRR)
		}
	}
}

// Функции парсинга (скопированы из userbot.go)
func isRegularSignal(text string) bool {
	t := strings.ToUpper(text)
	if strings.Contains(t, "GEM SIGNAL") {
		return false
	}
	if reProfLoss.MatchString(text) {
		return false
	}
	return reSignalID.MatchString(text) &&
		reEntry.MatchString(text) &&
		reTargets.MatchString(text) &&
		reSLLine.MatchString(text)
}

func parseSignalText(text string) (*ParsedSignal, bool) {
	if !isRegularSignal(text) {
		return nil, false
	}

	g := func(re *regexp.Regexp) string {
		m := re.FindStringSubmatch(text)
		if len(m) < 2 {
			return ""
		}
		return strings.TrimSpace(m[1])
	}

	id, _ := strconv.Atoi(g(reSignalID))

	// "$ALGO/USDT" → "ALGOUSDT"
	rawCoin := g(reCoin)
	symbol := strings.ReplaceAll(rawCoin, "$", "")
	symbol = strings.ReplaceAll(symbol, "/", "")

	dir := strings.ToUpper(g(reDirection))

	// Entry: "1.240 - 1.250"
	entryStr := g(reEntry)
	entryParts := splitFloats(entryStr)
	var entryLow, entryHigh float64
	if len(entryParts) >= 1 {
		entryLow = entryParts[0]
		entryHigh = entryParts[0]
	}
	if len(entryParts) >= 2 {
		entryHigh = entryParts[len(entryParts)-1]
	}

	// SL
	slStr := g(reSLLine)
	sl := parseFirstFloat(slStr)

	// TPs
	targetsStr := g(reTargets)
	tps := splitFloats(targetsStr)

	return &ParsedSignal{
		ID:        id,
		Symbol:    symbol,
		Direction: dir,
		EntryLow:  entryLow,
		EntryHigh: entryHigh,
		SL:        sl,
		TPs:       tps,
		Raw:       text,
	}, true
}

func splitFloats(s string) []float64 {
	var out []float64
	for _, part := range strings.Split(s, "-") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseFloat(part, 64)
		if err == nil && v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func parseFirstFloat(s string) float64 {
	parts := splitFloats(s)
	if len(parts) > 0 {
		return parts[0]
	}
	return 0
}

func formatSignalAlert(sig *ParsedSignal) string {
	mid := (sig.EntryLow + sig.EntryHigh) / 2
	base := strings.TrimSuffix(sig.Symbol, "USDT")

	slPct := 0.0
	if mid > 0 {
		slPct = (mid - sig.SL) / mid * 100
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "🚨 <b>Новый сигнал #%d</b>\n\n", sig.ID)
	fmt.Fprintf(&sb, "📊 <b>%s/USDT %s</b>\n\n", base, sig.Direction)
	fmt.Fprintf(&sb, "Вход: <code>$%g – $%g</code>\n", sig.EntryLow, sig.EntryHigh)
	fmt.Fprintf(&sb, "Стоп: <code>$%g</code> (−%.1f%%)\n", sig.SL, slPct)

	if len(sig.TPs) > 0 {
		sb.WriteString("\nТаргеты:\n")
		for i, tp := range sig.TPs {
			tpPct := 0.0
			if mid > 0 {
				tpPct = (tp - mid) / mid * 100
			}
			fmt.Fprintf(&sb, "  TP%d: <code>$%g</code> (+%.1f%%)\n", i+1, tp, tpPct)
		}
	}

	return sb.String()
}