// parse_demo — демонстрация парсинга реальных сообщений из Telegram канала
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
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

type Config struct {
	AppID           int
	AppHash         string
	Phone           string
	Trader3Channel  string
	Trader3ThreadID int
}

type LiveParser struct {
	channelID    int64
	threadID     int
	messageCount int
	signalCount  int
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

const sessionFile = "session_parse_demo.json"

func main() {
	fmt.Println("🎯 ДЕМОНСТРАЦИЯ ПАРСЕРА СИГНАЛОВ ИЗ РЕАЛЬНОГО TELEGRAM КАНАЛА")
	fmt.Println("=" + strings.Repeat("=", 65))
	
	// Загружаем конфигурацию
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: .env not found, using environment variables")
	}
	
	cfg := loadConfig()
	if cfg.AppID == 0 || cfg.AppHash == "" || cfg.Phone == "" {
		log.Fatal("❌ Set TELEGRAM_APP_ID, TELEGRAM_APP_HASH, TELEGRAM_PHONE in .env")
	}
	
	if cfg.Trader3Channel == "" {
		log.Fatal("❌ Set TRADER3_CHANNEL in .env")
	}
	
	parser := &LiveParser{
		channelID:    parseChannelID(cfg.Trader3Channel),
		threadID:     cfg.Trader3ThreadID,
		messageCount: 0,
		signalCount:  0,
	}
	
	ctx, cancel := context.WithCancel(context.Background())
	
	// Handle graceful shutdown
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		fmt.Println("\n\n🛑 Stopping parser...")
		cancel()
	}()
	
	fmt.Printf("🔌 Подключение к каналу %s (тред: %d)\n", cfg.Trader3Channel, cfg.Trader3ThreadID)
	fmt.Println("📡 Ожидание сообщений из канала...")
	fmt.Println("   (Нажмите Ctrl+C для завершения)")
	fmt.Println()
	
	err := parser.Run(ctx, cfg)
	if err != nil && err != context.Canceled {
		log.Fatalf("❌ Parser error: %v", err)
	}
	
	// Показываем итоговую статистику
	fmt.Println("\n" + strings.Repeat("=", 50))
	fmt.Println("📊 ИТОГОВАЯ СТАТИСТИКА:")
	fmt.Printf("   Всего сообщений: %d\n", parser.messageCount)
	fmt.Printf("   Распознано сигналов: %d\n", parser.signalCount)
	if parser.messageCount > 0 {
		rate := float64(parser.signalCount) / float64(parser.messageCount) * 100
		fmt.Printf("   Процент распознавания: %.1f%%\n", rate)
	}
	fmt.Println("✅ Демонстрация завершена!")
}

func loadConfig() *Config {
	appID, _ := strconv.Atoi(os.Getenv("TELEGRAM_APP_ID"))
	threadID, _ := strconv.Atoi(getenv("TRADER3_THREAD_ID", "8"))

	return &Config{
		AppID:           appID,
		AppHash:         os.Getenv("TELEGRAM_APP_HASH"),
		Phone:           os.Getenv("TELEGRAM_PHONE"),
		Trader3Channel:  getenv("TRADER3_CHANNEL", ""),
		Trader3ThreadID: threadID,
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseChannelID(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		log.Printf("TRADER3_CHANNEL=%q is not numeric — channel filter disabled", s)
		return 0
	}
	if id < 0 {
		id = -id
	}
	const prefix = int64(1_000_000_000_000)
	if id > prefix {
		id -= prefix
	}
	return id
}

func (p *LiveParser) Run(ctx context.Context, cfg *Config) error {
	dispatcher := tg.NewUpdateDispatcher()
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, upd *tg.UpdateNewChannelMessage) error {
		return p.handleMessage(upd)
	})

	client := telegram.NewClient(cfg.AppID, cfg.AppHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: sessionFile},
		UpdateHandler:  dispatcher,
	})

	return client.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(
			auth.Constant(cfg.Phone, "", &codeReader{}),
			auth.SendCodeOptions{},
		)
		if err := client.Auth().IfNecessary(ctx, flow); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
		
		fmt.Printf("✅ Авторизованы — мониторим канал ID=%d\n\n", p.channelID)

		<-ctx.Done()
		return ctx.Err()
	})
}

func (p *LiveParser) handleMessage(upd *tg.UpdateNewChannelMessage) error {
	msg, ok := upd.Message.(*tg.Message)
	if !ok || msg.Out {
		return nil
	}

	// Channel filter
	if p.channelID != 0 {
		peer, ok := msg.PeerID.(*tg.PeerChannel)
		if !ok || peer.ChannelID != p.channelID {
			return nil
		}
	}

	// Thread filter
	if p.threadID != 0 {
		replyTo, ok := msg.ReplyTo.(*tg.MessageReplyHeader)
		topID := 0
		if ok {
			if replyTo.ForumTopic {
				topID = replyTo.ReplyToMsgID
			} else {
				topID = replyTo.ReplyToTopID
			}
		}
		if topID != p.threadID {
			return nil
		}
	}

	text := msg.Message
	if text == "" {
		return nil
	}

	p.messageCount++
	
	fmt.Printf("📝 СООБЩЕНИЕ #%d (ID: %d, %d символов)\n", p.messageCount, msg.ID, len(text))
	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("```\n%s\n```\n", text)
	
	// Анализируем сообщение
	fmt.Println("\n🔍 АНАЛИЗ ПАРСИНГА:")
	
	// Проверяем фильтры
	fmt.Printf("1️⃣  Проверка фильтров: ")
	isValid, reason := checkSignalValid(text)
	if !isValid {
		fmt.Printf("❌ ОТФИЛЬТРОВАНО (%s)\n\n", reason)
		return nil
	}
	fmt.Printf("✅ Прошел все фильтры\n")
	
	// Показываем извлечение полей
	fmt.Printf("2️⃣  Извлечение полей:\n")
	showRegexMatches(text)
	
	// Парсим в структуру
	fmt.Printf("3️⃣  Парсинг в структуру:\n")
	sig, ok := parseSignalText(text)
	if !ok {
		fmt.Printf("   ❌ Ошибка парсинга\n\n")
		return nil
	}
	
	p.signalCount++
	
	// Показываем структуру
	showParsedStructure(sig)
	
	// Показываем алерт
	fmt.Printf("4️⃣  Готовый Telegram алерт:\n")
	alert := formatSignalAlert(sig)
	fmt.Printf("```\n%s\n```\n", alert)
	
	fmt.Printf("\n🎉 СИГНАЛ #%d УСПЕШНО СПАРШЕН!\n", sig.ID)
	fmt.Printf("📊 Статистика: %d/%d сообщений распознано (%.1f%%)\n", 
		p.signalCount, p.messageCount, float64(p.signalCount)/float64(p.messageCount)*100)
	fmt.Println()
	
	return nil
}

type codeReader struct{}

func (codeReader) Code(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
	fmt.Print("Введите код из Telegram: ")
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text()), nil
	}
	return "", fmt.Errorf("failed to read code from stdin")
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
			fmt.Printf("   ✅ %s: %q\n", r.name, strings.TrimSpace(match[1]))
		} else {
			fmt.Printf("   ❌ %s: не найден\n", r.name)
		}
	}
}

func showParsedStructure(sig *ParsedSignal) {
	fmt.Printf("   📊 ParsedSignal{\n")
	fmt.Printf("       ID:        %d\n", sig.ID)
	fmt.Printf("       Symbol:    %q\n", sig.Symbol)
	fmt.Printf("       Direction: %q\n", sig.Direction)
	fmt.Printf("       EntryLow:  %.6g\n", sig.EntryLow)
	fmt.Printf("       EntryHigh: %.6g\n", sig.EntryHigh)
	fmt.Printf("       SL:        %.6g\n", sig.SL)
	fmt.Printf("       TPs:       %v (%d targets)\n", sig.TPs, len(sig.TPs))
	fmt.Printf("   }\n")
	
	// Вычисления
	mid := (sig.EntryLow + sig.EntryHigh) / 2
	if mid > 0 {
		slPct := (mid - sig.SL) / mid * 100
		fmt.Printf("\n   📈 Расчеты:\n")
		fmt.Printf("       Entry Mid: %.6g\n", mid)
		fmt.Printf("       SL Risk:   -%.2f%%\n", slPct)
		
		if len(sig.TPs) > 0 {
			fmt.Printf("       TP Rewards:\n")
			for i, tp := range sig.TPs {
				tpPct := (tp - mid) / mid * 100
				rr := tpPct / slPct
				fmt.Printf("         TP%d: %.6g (+%.2f%%, R:R %.2f:1)\n", i+1, tp, tpPct, rr)
			}
		}
	}
}

// Копируем функции парсинга из userbot.go
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