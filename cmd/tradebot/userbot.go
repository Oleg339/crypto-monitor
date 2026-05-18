package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
)

const sessionFile = "session_userbot.json"

// ── Types ─────────────────────────────────────────────────────────────────────

type Userbot struct {
	appID     int
	appHash   string
	phone     string
	channelID int64 // bare MTProto channel ID (without -100 prefix)
	threadID  int   // forum topic ID (0 = any)
	bot       *Bot
}

type ParsedSignal struct {
	ID        int
	Symbol    string
	Direction string
	EntryLow  float64
	EntryHigh float64
	SL        float64
	TPs       []float64
}

// ── Constructor ───────────────────────────────────────────────────────────────

func newUserbot(cfg *Config, bot *Bot) *Userbot {
	return &Userbot{
		appID:     cfg.AppID,
		appHash:   cfg.AppHash,
		phone:     cfg.Phone,
		channelID: parseChannelID(cfg.Trader3Channel),
		threadID:  cfg.Trader3ThreadID,
		bot:       bot,
	}
}

// parseChannelID converts various formats to the bare MTProto channel ID.
//   -1003722628653  →  3722628653
//    1003722628653  →  3722628653  (strip 1_000_000_000_000 prefix)
//    3722628653     →  3722628653  (already bare)
func parseChannelID(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		log.Printf("[userbot] TRADER3_CHANNEL=%q is not numeric — channel filter disabled", s)
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

// ── Run ───────────────────────────────────────────────────────────────────────

func (u *Userbot) Run(ctx context.Context) error {
	dispatcher := tg.NewUpdateDispatcher()
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, upd *tg.UpdateNewChannelMessage) error {
		return u.handleMsg(upd)
	})

	client := telegram.NewClient(u.appID, u.appHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: sessionFile},
		UpdateHandler:  dispatcher,
	})

	return client.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(
			auth.Constant(u.phone, "", codeReader{}),
			auth.SendCodeOptions{},
		)
		if err := client.Auth().IfNecessary(ctx, flow); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
		log.Printf("[userbot] authenticated — monitoring channel ID=%d", u.channelID)

		<-ctx.Done()
		return ctx.Err()
	})
}

// codeReader reads the Telegram login code from stdin.
type codeReader struct{}

func (codeReader) Code(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
	fmt.Print("[userbot] Enter Telegram code: ")
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text()), nil
	}
	return "", fmt.Errorf("failed to read code from stdin")
}

// ── Message handler ───────────────────────────────────────────────────────────

func (u *Userbot) handleMsg(upd *tg.UpdateNewChannelMessage) error {
	msg, ok := upd.Message.(*tg.Message)
	if !ok || msg.Out {
		return nil
	}

	// Channel filter
	if u.channelID != 0 {
		peer, ok := msg.PeerID.(*tg.PeerChannel)
		if !ok || peer.ChannelID != u.channelID {
			return nil
		}
	}

	// Thread (topic) filter
	if u.threadID != 0 {
		replyTo, ok := msg.ReplyTo.(*tg.MessageReplyHeader)
		topID := 0
		if ok {
			if replyTo.ForumTopic {
				topID = replyTo.ReplyToMsgID
			} else {
				topID = replyTo.ReplyToTopID
			}
		}
		if topID != u.threadID {
			return nil
		}
	}

	text := msg.Message
	if text == "" {
		return nil
	}

	log.Printf("[userbot] ← msg id=%d len=%d", msg.ID, len(text))

	sig, ok := parseSignalText(text)
	if !ok {
		return nil
	}

	log.Printf("[userbot] new signal #%d %s %s  entry=%.4g–%.4g  sl=%.4g  tps=%d",
		sig.ID, sig.Symbol, sig.Direction,
		sig.EntryLow, sig.EntryHigh, sig.SL, len(sig.TPs))

	u.bot.SendSignalFromUserbot(sig)
	return nil
}

// ── Signal parsing ────────────────────────────────────────────────────────────

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

// ── Alert formatting ──────────────────────────────────────────────────────────

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
