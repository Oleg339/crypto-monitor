// parser_tester — standalone tool to test signal parsing from real Telegram channel
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
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
)

const sessionFile = "session_parser_tester.json"

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: .env not found, using environment variables")
	}

	cfg := loadConfig()
	if cfg.AppID == 0 || cfg.AppHash == "" || cfg.Phone == "" {
		log.Fatal("Set TELEGRAM_APP_ID, TELEGRAM_APP_HASH, TELEGRAM_PHONE in .env")
	}

	parser := &SignalParser{
		channelID: parseChannelID(cfg.Trader3Channel),
		threadID:  cfg.Trader3ThreadID,
	}

	ctx, cancel := context.WithCancel(context.Background())
	
	// Handle graceful shutdown
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		log.Println("Shutting down...")
		cancel()
	}()

	log.Printf("Starting parser for channel %s (thread: %d)", cfg.Trader3Channel, cfg.Trader3ThreadID)
	log.Println("Press Ctrl+C to stop and see results")
	
	err := parser.Run(ctx, cfg)
	if err != nil && err != context.Canceled {
		log.Fatalf("Parser error: %v", err)
	}

	// Print results
	parser.PrintResults()
}

type Config struct {
	AppID           int
	AppHash         string
	Phone           string
	Trader3Channel  string
	Trader3ThreadID int
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

type SignalParser struct {
	channelID int64
	threadID  int
	signals   []ParsedSignal
	messages  []MessageInfo
}

type MessageInfo struct {
	ID        int
	Text      string
	Timestamp time.Time
	Parsed    bool
}

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

func (p *SignalParser) Run(ctx context.Context, cfg *Config) error {
	dispatcher := tg.NewUpdateDispatcher()
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, upd *tg.UpdateNewChannelMessage) error {
		return p.handleMsg(upd)
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
		
		log.Printf("✅ Authenticated — monitoring channel ID=%d", p.channelID)
		log.Println("Waiting for messages...")

		<-ctx.Done()
		return ctx.Err()
	})
}

func (p *SignalParser) handleMsg(upd *tg.UpdateNewChannelMessage) error {
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

	msgInfo := MessageInfo{
		ID:        msg.ID,
		Text:      text,
		Timestamp: time.Now(),
		Parsed:    false,
	}

	// Try to parse as signal
	if sig, ok := parseSignalText(text); ok {
		sig.Raw = text
		p.signals = append(p.signals, *sig)
		msgInfo.Parsed = true
		
		log.Printf("🎯 NEW SIGNAL #%d: %s %s (Entry: %.6g-%.6g, SL: %.6g, TPs: %d)", 
			sig.ID, sig.Symbol, sig.Direction, sig.EntryLow, sig.EntryHigh, sig.SL, len(sig.TPs))
	} else {
		log.Printf("📝 Message #%d: %s... (not a signal)", msg.ID, truncate(text, 50))
	}

	p.messages = append(p.messages, msgInfo)
	return nil
}

func (p *SignalParser) PrintResults() {
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("🔍 PARSING RESULTS")
	fmt.Println(strings.Repeat("=", 60))
	
	fmt.Printf("📊 Statistics:\n")
	fmt.Printf("  Total messages: %d\n", len(p.messages))
	fmt.Printf("  Valid signals:  %d\n", len(p.signals))
	
	if len(p.messages) > 0 {
		parseRate := float64(len(p.signals)) / float64(len(p.messages)) * 100
		fmt.Printf("  Parse rate:     %.1f%%\n", parseRate)
	}
	
	fmt.Println()

	// Show all messages
	fmt.Println("📝 ALL MESSAGES:")
	fmt.Println(strings.Repeat("-", 40))
	for i, msg := range p.messages {
		status := "❌ Not parsed"
		if msg.Parsed {
			status = "✅ Parsed"
		}
		fmt.Printf("\n[%d] %s %s\n", i+1, msg.Timestamp.Format("15:04:05"), status)
		fmt.Printf("ID: %d\n", msg.ID)
		fmt.Printf("Text: %s\n", truncate(msg.Text, 200))
	}

	// Show parsed signals
	if len(p.signals) > 0 {
		fmt.Println("\n🎯 PARSED SIGNALS:")
		fmt.Println(strings.Repeat("-", 40))
		
		for i, sig := range p.signals {
			fmt.Printf("\n[%d] Signal #%d - %s %s\n", i+1, sig.ID, sig.Symbol, sig.Direction)
			fmt.Printf("    Entry: %.6g - %.6g\n", sig.EntryLow, sig.EntryHigh)
			fmt.Printf("    SL:    %.6g\n", sig.SL)
			fmt.Printf("    TPs:   %v\n", sig.TPs)
			
			// Calculate percentages
			mid := (sig.EntryLow + sig.EntryHigh) / 2
			if mid > 0 {
				slPct := (mid - sig.SL) / mid * 100
				fmt.Printf("    SL%%:   -%.1f%%\n", slPct)
				
				if len(sig.TPs) > 0 {
					fmt.Printf("    TP%%s:  ")
					for j, tp := range sig.TPs {
						tpPct := (tp - mid) / mid * 100
						if j > 0 {
							fmt.Printf(", ")
						}
						fmt.Printf("TP%d: +%.1f%%", j+1, tpPct)
					}
					fmt.Println()
				}
			}
			
			// Show formatted alert
			alert := formatSignalAlert(&sig)
			fmt.Printf("\n📱 Formatted Alert:\n%s\n", alert)
		}
	} else {
		fmt.Println("\n⚠️  No valid signals found in the messages")
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// Parsing functions from userbot.go
// (copying the exact same functions to maintain consistency)

var (
	reSignalID  = regexp.MustCompile(`(?i)SIGNAL\s*ID\s*:\s*#(\d+)`)
	reCoin      = regexp.MustCompile(`(?i)COIN\s*:\s*(\S+)`)
	reDirection = regexp.MustCompile(`(?i)Direction\s*:\s*(LONG|SHORT)`)
	reEntry     = regexp.MustCompile(`(?i)ENTRY\s*:\s*([^\n]+)`)
	reTargets   = regexp.MustCompile(`(?i)TARGETS\s*:\s*([^\n]+)`)
	reSLLine    = regexp.MustCompile(`(?i)STOP\s*LOSS\s*:\s*([^\n]+)`)
	reProfLoss  = regexp.MustCompile(`(?i)\d[\d.,]*\s*%\s*(PROFIT|LOSS)`)
)

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

type codeReader struct{}

func (codeReader) Code(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
	fmt.Print("Enter Telegram code: ")
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text()), nil
	}
	return "", fmt.Errorf("failed to read code from stdin")
}