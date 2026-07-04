package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yourorg/crypto-monitor/internal/bybit"
)

// ── Telegram types ────────────────────────────────────────────────────────────

type tgUpdate struct {
	UpdateID      int         `json:"update_id"`
	Message       *tgMessage  `json:"message"`
	CallbackQuery *tgCallback `json:"callback_query"`
}

type tgMessage struct {
	MessageID int    `json:"message_id"`
	Chat      tgChat `json:"chat"`
	Text      string `json:"text"`
}

type tgChat struct {
	ID int64 `json:"id"`
}

type tgCallback struct {
	ID      string     `json:"id"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}

// ── Bot ───────────────────────────────────────────────────────────────────────

type Bot struct {
	token   string
	chatID  int64
	bybit   *BybitClient
	cfg     *Config
	paper   *PaperStore // nil when live mode
	monitor *Monitor    // nil when live mode
	userbot *Userbot    // set after userbot is created
	hc      *http.Client

	mu           sync.Mutex
	pending      map[string]*pendingSignal
	offset       int
	paused       bool
	pauseReason  string
	hideEquity   bool // when true: show wallet balance only, not equity with unrealised PnL
	auto         bool // авторежим: исполнять сигналы без кнопки подтверждения
}

// IsAuto — включён ли авторежим. Источник истины — bot_settings в общей БД
// (её же переключает тумблер в webapp); без БД — поле в памяти.
func (b *Bot) IsAuto() bool {
	if b.paper != nil {
		if v, err := b.paper.GetSetting(context.Background(), "auto_trade"); err == nil && v != "" {
			return v == "1"
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.auto
}

func (b *Bot) setAuto(on bool) {
	b.mu.Lock()
	b.auto = on
	b.mu.Unlock()
	if b.paper != nil {
		v := "0"
		if on {
			v = "1"
		}
		if err := b.paper.SetSetting(context.Background(), "auto_trade", v); err != nil {
			log.Printf("[settings] set auto_trade: %v", err)
		}
	}
	log.Printf("[bot] auto mode: %v", on)
}

func (b *Bot) Pause(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.paused = true
	b.pauseReason = reason
}

func (b *Bot) Resume() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.paused = false
	b.pauseReason = ""
}

func (b *Bot) IsPaused() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.paused
}

type pendingSignal struct {
	Symbol    string
	Side      string
	EntryLow  float64
	EntryHigh float64
	EntryMid  float64
	SL        float64
	TP        float64
	QtyStr    string
	PricePrec int
	Margin    float64
	Leverage  float64
	Qty       float64
	DBID      int64 // ID в received_signals, 0 для ручных сигналов
}

func newBot(cfg *Config, bybitClient *BybitClient, paper *PaperStore) *Bot {
	dialer := bybit.NewResilientDialer()
	dialer.KeepWarm("api.telegram.org", 5*time.Minute)
	return &Bot{
		token:  cfg.TGToken,
		chatID: cfg.TGChatID,
		bybit:  bybitClient,
		cfg:    cfg,
		paper:  paper,
		hc: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext: dialer.DialContext,
			},
		},
		pending: map[string]*pendingSignal{},
		auto:    cfg.AutoTrade,
	}
}

// ── Poll loop ─────────────────────────────────────────────────────────────────

func (b *Bot) Run(ctx context.Context) {
	b.setupWebApp()
	log.Println("[tg] long-polling started")
	for {
		select {
		case <-ctx.Done():
			log.Println("[tg] polling stopped")
			return
		default:
		}

		updates, err := b.getUpdates(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[tg] getUpdates: %v", err)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}

		for _, u := range updates {
			b.offset = u.UpdateID + 1
			if u.Message != nil && u.Message.Chat.ID == b.chatID {
				log.Printf("[tg] ← msg  chat=%d  text=%q", u.Message.Chat.ID, u.Message.Text)
				b.handleMessage(u.Message)
			}
			if u.CallbackQuery != nil {
				log.Printf("[tg] ← cb   id=%s  data=%q", u.CallbackQuery.ID, u.CallbackQuery.Data)
				b.handleCallback(u.CallbackQuery)
			}
		}
	}
}

// setupWebApp installs the Mini App menu button if WEBAPP_URL is set.
func (b *Bot) setupWebApp() {
	url := os.Getenv("WEBAPP_URL")
	if url == "" {
		return
	}
	b.tgPost("setChatMenuButton", map[string]interface{}{
		"menu_button": map[string]interface{}{
			"type":    "web_app",
			"text":    "📊 Панель",
			"web_app": map[string]string{"url": url},
		},
	})
}

func (b *Bot) getUpdates(ctx context.Context) ([]tgUpdate, error) {
	url := fmt.Sprintf(
		"https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10&allowed_updates=[\"message\",\"callback_query\"]",
		b.token, b.offset)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := b.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var res struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return res.Result, nil
}

// ── Message router ────────────────────────────────────────────────────────────

func (b *Bot) handleMessage(msg *tgMessage) {
	text := strings.TrimSpace(msg.Text)
	cmd, rest, _ := strings.Cut(text, " ")
	cmd = strings.ToLower(cmd)
	cmd = strings.TrimPrefix(cmd, "/")
	cmd, _, _ = strings.Cut(cmd, "@") // strip @botname

	switch cmd {
	case "start", "help":
		extra := ""
		if b.cfg.PaperTrading {
			extra = "\n<code>/stats</code> — статистика paper trading"
		}
		b.send(msg.Chat.ID, 0,
			"📖 <b>Команды:</b>\n\n"+
				"<code>/balance</code> — баланс аккаунта\n"+
				"<code>/signal SYMBOL entry_low entry_high sl tp</code>\n"+
				"<code>/positions</code> — открытые позиции\n"+
				"<code>/signals</code> — сигналы за последние 24ч\n"+
				"<code>/auto</code> — вкл/выкл автоисполнение сигналов\n"+
				"<code>/cancel</code> — отменить все ордера"+
				extra+"\n\n"+
				"Пример:\n<code>/signal AVAXUSDT 9.20 9.30 8.50 11.50</code>", nil)
	case "balance":
		b.cmdBalance(msg)
	case "signal":
		b.cmdSignal(msg, strings.TrimSpace(rest))
	case "positions":
		b.cmdPositions(msg)
	case "cancel":
		b.cmdCancel(msg)
	case "stats":
		b.cmdStats(msg)
	case "resume":
		b.cmdResume(msg)
	case "signals":
		b.cmdSignals(msg)
	case "auto":
		b.cmdAuto(msg)
	}
}

// ── /auto ─────────────────────────────────────────────────────────────────────

func (b *Bot) cmdAuto(msg *tgMessage) {
	on := !b.IsAuto()
	b.setAuto(on)

	if on {
		note := "<i>После рестарта бот вернётся к значению AUTO_TRADE из .env</i>"
		if b.paper != nil {
			note = "<i>Состояние сохранено в БД — рестарт его не сбросит. Та же кнопка есть в панели.</i>"
		}
		filter := ""
		if b.cfg.BTCHighSkipPct > 0 {
			filter = fmt.Sprintf("\nФильтр: лонги при BTC ближе %.0f%% к 10-дневному максимуму — только вручную.", b.cfg.BTCHighSkipPct)
		}
		b.send(msg.Chat.ID, 0,
			"🤖 <b>Авторежим включён</b> — новые сигналы исполняются сразу, без кнопки подтверждения."+filter+"\n\n"+
				"Выключить: /auto\n"+note, nil)
	} else {
		b.send(msg.Chat.ID, 0,
			"👤 <b>Авторежим выключен</b> — сигналы снова требуют подтверждения кнопкой.", nil)
	}
}

// ── /balance ──────────────────────────────────────────────────────────────────

func (b *Bot) cmdBalance(msg *tgMessage) {
	wi, err := b.bybit.Balance()
	if err != nil {
		b.send(msg.Chat.ID, 0, "❌ "+esc(err.Error()), nil)
		return
	}
	b.mu.Lock()
	hide := b.hideEquity
	b.mu.Unlock()

	text := b.formatBalance(wi, hide)
	kb := b.balanceKB(hide)
	b.send(msg.Chat.ID, 0, text, kb)
}

func (b *Bot) formatBalance(wi *WalletInfo, hideEquity bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "💰 <b>Unified Account</b>\n\n")

	if hideEquity {
		// Show wallet balance only — unrealised PnL excluded
		fmt.Fprintf(&sb, "Баланс:    <code>$%.2f</code>\n", wi.TotalWalletBalance)
		fmt.Fprintf(&sb, "Available: <code>$%.2f</code>\n", wi.TotalAvailBalance)
		if wi.TotalUnrealisedPnl != 0 {
			fmt.Fprintf(&sb, "\n<i>Unrealised PnL скрыт</i>\n")
		}
	} else {
		// Default: equity first, wallet below for reference
		fmt.Fprintf(&sb, "Equity:    <code>$%.2f</code>\n", wi.TotalEquity)
		fmt.Fprintf(&sb, "Баланс:    <code>$%.2f</code>\n", wi.TotalWalletBalance)
		fmt.Fprintf(&sb, "Available: <code>$%.2f</code>\n", wi.TotalAvailBalance)
		if wi.TotalUnrealisedPnl != 0 {
			sign := "+"
			if wi.TotalUnrealisedPnl < 0 {
				sign = ""
			}
			fmt.Fprintf(&sb, "Unrealised PnL: <code>%s$%.2f</code>\n",
				sign, wi.TotalUnrealisedPnl)
		}
	}

	if len(wi.Coins) > 0 {
		sb.WriteString("\n<b>Монеты:</b>\n")
		for _, c := range wi.Coins {
			if !hideEquity && c.UnrealisedPnl != 0 {
				sign := "+"
				if c.UnrealisedPnl < 0 {
					sign = ""
				}
				fmt.Fprintf(&sb, "• %s: <code>%.6g</code> (PnL: %s%.2f)\n",
					c.Coin, c.WalletBalance, sign, c.UnrealisedPnl)
			} else {
				fmt.Fprintf(&sb, "• %s: <code>%.6g</code>\n", c.Coin, c.WalletBalance)
			}
		}
	}
	return sb.String()
}

func (b *Bot) balanceKB(hideEquity bool) json.RawMessage {
	label := "🙈 Скрыть PnL"
	if hideEquity {
		label = "👁 Показать PnL"
	}
	return inlineKB([][]kbBtn{{{Text: label, Data: "balance:toggle_equity"}}})
}

// ── /signal ───────────────────────────────────────────────────────────────────

func (b *Bot) cmdSignal(msg *tgMessage, args string) {
	parts := strings.Fields(args)
	if len(parts) < 5 {
		b.send(msg.Chat.ID, 0,
			"⚠️ Формат: <code>/signal SYMBOL entry_low entry_high sl tp</code>\n"+
				"Пример: <code>/signal AVAXUSDT 9.20 9.30 8.50 11.50</code>", nil)
		return
	}

	symbol := strings.ToUpper(parts[0])
	entryLow := parg(parts[1])
	entryHigh := parg(parts[2])
	sl := parg(parts[3])
	tp := parg(parts[4])

	if entryLow <= 0 || entryHigh <= 0 || sl <= 0 || tp <= 0 {
		b.send(msg.Chat.ID, 0, "❌ Некорректные числа", nil)
		return
	}
	if entryLow >= entryHigh {
		b.send(msg.Chat.ID, 0, "❌ entry_low должен быть меньше entry_high", nil)
		return
	}
	if sl >= entryLow {
		b.send(msg.Chat.ID, 0, "❌ stop_loss должен быть ниже entry_low", nil)
		return
	}
	if tp <= entryHigh {
		b.send(msg.Chat.ID, 0, "❌ take_profit должен быть выше entry_high", nil)
		return
	}

	// Fetch live equity and instrument info in parallel
	type balResult struct {
		equity float64
		err    error
	}
	type infoResult struct {
		info *InstrumentInfo
		err  error
	}
	balCh := make(chan balResult, 1)
	infoCh := make(chan infoResult, 1)

	go func() {
		wi, err := b.bybit.Balance()
		if err != nil {
			balCh <- balResult{0, err}
			return
		}
		balCh <- balResult{wi.TotalEquity, nil}
	}()
	go func() {
		info, err := b.bybit.InstrumentInfo(symbol)
		infoCh <- infoResult{info, err}
	}()

	balRes := <-balCh
	if balRes.err != nil {
		b.send(msg.Chat.ID, 0, "❌ Не удалось получить баланс: <code>"+esc(balRes.err.Error())+"</code>", nil)
		return
	}
	infoRes := <-infoCh
	if infoRes.err != nil {
		b.send(msg.Chat.ID, 0, "❌ Инструмент не найден: <code>"+esc(symbol)+"</code>", nil)
		return
	}

	equity := balRes.equity
	info := infoRes.info

	entryMid := (entryLow + entryHigh) / 2.0
	qtyPrec := decimals(info.QtyStep)
	pricePrec := decimals(info.TickSize)

	// Position sizing:
	//   margin   = equity × risk%        (collateral you put up)
	//   notional = margin × leverage     (total position value)
	//   qty      = notional / entry_mid
	margin := equity * b.cfg.RiskPct / 100
	notional := margin * b.cfg.Leverage
	qty := roundStep(notional/entryMid, info.QtyStep)
	if qty < info.MinOrderQty {
		qty = info.MinOrderQty
	}
	qtyStr := ff(qty, qtyPrec)

	slPct := (entryMid - sl) / entryMid * 100
	tpPct := (tp - entryMid) / entryMid * 100
	rr := 0.0
	if slPct != 0 {
		rr = tpPct / slPct
	}

	base := strings.TrimSuffix(symbol, "USDT")

	// Store pending signal
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	b.mu.Lock()
	b.pending[id] = &pendingSignal{
		Symbol:    symbol,
		Side:      "Buy",
		EntryLow:  entryLow,
		EntryHigh: entryHigh,
		EntryMid:  entryMid,
		SL:        sl,
		TP:        tp,
		QtyStr:    qtyStr,
		PricePrec: pricePrec,
		Margin:    margin,
		Leverage:  b.cfg.Leverage,
		Qty:       qty,
	}
	b.mu.Unlock()

	text := fmt.Sprintf(
		"📊 <b>%s/USDT LONG</b>\n\n"+
			"Вход: <code>$%s – $%s</code> (mid: <code>$%s</code>)\n"+
			"Стоп: <code>$%s</code> (−%.1f%%)\n"+
			"TP:   <code>$%s</code> (+%.1f%%)\n"+
			"R:R:  <code>%.1f:1</code>\n\n"+
			"💰 <b>Позиция</b> (%.0f%% от $%.2f, плечо %.0fx):\n"+
			"Количество: <code>%s %s</code>\n"+
			"Маржа: <code>$%.2f</code>",
		base,
		ff(entryLow, pricePrec), ff(entryHigh, pricePrec), ff(entryMid, pricePrec),
		ff(sl, pricePrec), slPct,
		ff(tp, pricePrec), tpPct,
		rr,
		b.cfg.RiskPct, equity, b.cfg.Leverage,
		qtyStr, base,
		margin,
	)

	kb := inlineKB([][]kbBtn{{{
		Text: "✅ Войти", Data: "confirm:" + id,
	}, {
		Text: "❌ Отмена", Data: "cancel:" + id,
	}}})

	b.send(msg.Chat.ID, 0, text, kb)
}

// ── /positions ────────────────────────────────────────────────────────────────

func (b *Bot) cmdPositions(msg *tgMessage) {
	if b.cfg.PaperTrading {
		b.cmdPaperPositions(msg)
		return
	}

	positions, err := b.bybit.Positions()
	if err != nil {
		b.send(msg.Chat.ID, 0, "❌ "+esc(err.Error()), nil)
		return
	}
	if len(positions) == 0 {
		b.send(msg.Chat.ID, 0, "📭 Открытых позиций нет", nil)
		return
	}

	var sb strings.Builder
	sb.WriteString("📈 <b>Открытые позиции:</b>\n\n")
	for _, p := range positions {
		base := strings.TrimSuffix(p.Symbol, "USDT")
		pnlSign := ""
		if p.UnrealisedPnl > 0 {
			pnlSign = "+"
		}
		fmt.Fprintf(&sb,
			"<b>%s</b> %s  <code>%g @ $%g</code>  %.0fx\n"+
				"  PnL: <code>%s$%.2f</code>  Mark: <code>$%g</code>\n\n",
			base, p.Side, p.Size, p.EntryPrice, p.Leverage,
			pnlSign, p.UnrealisedPnl, p.MarkPrice)
	}
	b.send(msg.Chat.ID, 0, sb.String(), nil)
}

func (b *Bot) cmdPaperPositions(msg *tgMessage) {
	trades, err := b.paper.OpenTrades(context.Background())
	if err != nil {
		b.send(msg.Chat.ID, 0, "❌ "+esc(err.Error()), nil)
		return
	}
	if len(trades) == 0 {
		b.send(msg.Chat.ID, 0, "📭 [PAPER] Открытых позиций нет", nil)
		return
	}

	var sb strings.Builder
	sb.WriteString("📝 <b>[PAPER] Открытые позиции:</b>\n\n")
	for _, t := range trades {
		base := strings.TrimSuffix(t.Symbol, "USDT")
		fmt.Fprintf(&sb,
			"<b>%s</b> LONG  <code>%g @ $%g</code>  %.0fx\n"+
				"  SL: <code>$%g</code>  TP: <code>$%g</code>  ID: %d\n\n",
			base, t.Qty, t.EntryPrice, t.Leverage, t.SL, t.TP, t.ID)
	}
	b.send(msg.Chat.ID, 0, sb.String(), nil)
}

// ── /stats live ───────────────────────────────────────────────────────────────

func (b *Bot) cmdLiveStats(msg *tgMessage) {
	trades, err := b.bybit.ClosedPnl(50)
	if err != nil {
		b.send(msg.Chat.ID, 0, "❌ "+esc(err.Error()), nil)
		return
	}
	if len(trades) == 0 {
		b.send(msg.Chat.ID, 0, "📊 Закрытых сделок нет", nil)
		return
	}

	var totalPnl float64
	wins := 0
	for _, t := range trades {
		totalPnl += t.ClosedPnl
		if t.ClosedPnl > 0 {
			wins++
		}
	}
	total := len(trades)
	winRate := float64(wins) / float64(total) * 100
	avgPnl := totalPnl / float64(total)

	pnlSign := "+"
	if totalPnl < 0 {
		pnlSign = ""
	}
	avgSign := "+"
	if avgPnl < 0 {
		avgSign = ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "📊 <b>Live статистика</b> (последние %d)\n\n", total)
	fmt.Fprintf(&sb, "WinRate:  <code>%.1f%%</code>  (%d/%d)\n", winRate, wins, total)
	fmt.Fprintf(&sb, "Avg PnL:  <code>%s$%.2f</code>\n", avgSign, avgPnl)
	fmt.Fprintf(&sb, "Total PnL: <code>%s$%.2f</code>\n", pnlSign, totalPnl)

	// Last 5 trades
	sb.WriteString("\n<b>Последние сделки:</b>\n")
	for i, t := range trades {
		if i >= 5 {
			break
		}
		base := strings.TrimSuffix(t.Symbol, "USDT")
		sign := "+"
		if t.ClosedPnl < 0 {
			sign = ""
		}
		emoji := "✅"
		if t.ClosedPnl < 0 {
			emoji = "❌"
		}
		fmt.Fprintf(&sb, "%s %s  <code>%s$%.2f</code>  <i>%s</i>\n",
			emoji, base, sign, t.ClosedPnl,
			t.ClosedAt.Format("02 Jan 15:04"))
	}

	b.send(msg.Chat.ID, 0, sb.String(), nil)
}

// ── /stats ────────────────────────────────────────────────────────────────────

func (b *Bot) cmdStats(msg *tgMessage) {
	if !b.cfg.PaperTrading {
		b.cmdLiveStats(msg)
		return
	}

	ctx := context.Background()
	stats, err := b.paper.Stats(ctx)
	if err != nil {
		b.send(msg.Chat.ID, 0, "❌ "+esc(err.Error()), nil)
		return
	}

	if stats.Total == 0 && stats.OpenCount == 0 {
		b.send(msg.Chat.ID, 0, "📊 [PAPER] Сделок пока нет", nil)
		return
	}

	// Rolling winrate (last 10)
	rolling, _ := b.paper.RecentClosedTrades(ctx, rollingWindow)
	rollingWR := 0.0
	if len(rolling) >= rollingWindow {
		wins := 0
		for _, t := range rolling {
			if t.PnL != nil && *t.PnL > 0 {
				wins++
			}
		}
		rollingWR = float64(wins) / float64(len(rolling)) * 100
	}

	// Current equity
	equity := b.cfg.PaperInitialEquity + stats.TotalPnL
	equitySign := "+"
	if stats.TotalPnL < 0 {
		equitySign = ""
	}
	pnlSign := equitySign

	// Backtest comparison
	var btLine string
	if b.cfg.BacktestWinRate > 0 {
		wrDiff := stats.WinRate - b.cfg.BacktestWinRate
		pnlDiff := stats.AvgPnL - b.cfg.BacktestAvgPnL
		wrSign := "+"
		if wrDiff < 0 {
			wrSign = ""
		}
		pnlDiffSign := "+"
		if pnlDiff < 0 {
			pnlDiffSign = ""
		}
		btLine = fmt.Sprintf(
			"\n📐 <b>vs Бэктест</b>\n"+
				"WR: <code>%s%.1f%%</code> от %.1f%%\n"+
				"AvgPnL: <code>%s$%.2f</code> от $%.2f",
			wrSign, wrDiff, b.cfg.BacktestWinRate,
			pnlDiffSign, pnlDiff, b.cfg.BacktestAvgPnL)
	}

	// Pause status
	b.mu.Lock()
	paused := b.paused
	pauseReason := b.pauseReason
	b.mu.Unlock()

	statusLine := "🟢 Активен"
	if paused {
		statusLine = "⏸ На паузе: " + pauseReason
	}

	rollingLine := ""
	if len(rolling) >= rollingWindow {
		emoji := "✅"
		if rollingWR < 30 {
			emoji = "🔴"
		} else if rollingWR < 50 {
			emoji = "⚠️"
		}
		rollingLine = fmt.Sprintf("\n%s WR(10): <b>%.0f%%</b>", emoji, rollingWR)
	}

	text := fmt.Sprintf(
		"📊 <b>Paper Trading Stats</b>\n"+
			"<i>%s</i>\n\n"+
			"Закрытых: <b>%d</b>  (✅%d / ❌%d)\n"+
			"Win Rate:  <b>%.1f%%</b>%s\n"+
			"Avg PnL:   <code>%s$%.2f</code>\n"+
			"Total PnL: <code>%s$%.2f</code>\n"+
			"Equity:    <code>$%.2f</code>\n"+
			"Открытых:  <b>%d</b>%s",
		statusLine,
		stats.Total, stats.Wins, stats.Losses,
		stats.WinRate, rollingLine,
		pnlSign, stats.AvgPnL,
		equitySign, stats.TotalPnL,
		equity,
		stats.OpenCount,
		btLine,
	)
	b.send(msg.Chat.ID, 0, text, nil)
}

// SendSignalFromUserbot is called by the userbot when a new signal arrives.
// Strategy: ExitTP3 — 100% of position closed at TP3.
// If signal has fewer than 3 TPs, sends a plain alert without confirm button.
func (b *Bot) SendSignalFromUserbot(sig *ParsedSignal) {
	if len(sig.TPs) < 3 || sig.EntryLow == 0 || sig.SL == 0 {
		b.send(b.chatID, 0, formatSignalAlert(sig)+"\n\n⚠️ <i>TP3 недоступен — торговля не предложена</i>", nil)
		return
	}

	// Save to DB first (if paper store available)
	var dbID int64
	if b.paper != nil {
		var err error
		dbID, err = b.paper.SaveSignal(context.Background(), sig)
		if err != nil {
			log.Printf("[signals] save signal: %v", err)
		}
	}

	b.sendSignalMsg(sig, dbID, true)
}

// sendSignalMsg builds and sends an interactive signal message with confirm/cancel buttons.
// If dbID > 0, the pending key is "rs{dbID}" so callbacks can update DB status.
// allowAuto разрешает исполнение без подтверждения в авторежиме — true только
// для свежих сигналов из канала; перевысылки истории (/signals) всегда с кнопками.
func (b *Bot) sendSignalMsg(sig *ParsedSignal, dbID int64, allowAuto bool) {
	tp := sig.TPs[2]

	type balResult struct {
		equity float64
		err    error
	}
	type infoResult struct {
		info *InstrumentInfo
		err  error
	}
	balCh := make(chan balResult, 1)
	infoCh := make(chan infoResult, 1)

	go func() {
		wi, err := b.bybit.Balance()
		if err != nil {
			balCh <- balResult{0, err}
			return
		}
		balCh <- balResult{wi.TotalEquity, nil}
	}()
	go func() {
		info, err := b.bybit.InstrumentInfo(sig.Symbol)
		infoCh <- infoResult{info, err}
	}()

	balRes := <-balCh
	infoRes := <-infoCh

	if balRes.err != nil || infoRes.err != nil {
		b.send(b.chatID, 0, formatSignalAlert(sig), nil)
		return
	}

	equity := balRes.equity
	info := infoRes.info
	entryMid := (sig.EntryLow + sig.EntryHigh) / 2.0
	qtyPrec := decimals(info.QtyStep)
	pricePrec := decimals(info.TickSize)

	margin := equity * b.cfg.RiskPct / 100
	notional := margin * b.cfg.Leverage
	qty := roundStep(notional/entryMid, info.QtyStep)
	if qty < info.MinOrderQty {
		qty = info.MinOrderQty
	}
	qtyStr := ff(qty, qtyPrec)

	slPct := (entryMid - sig.SL) / entryMid * 100
	tpPct := (tp - entryMid) / entryMid * 100
	rr := 0.0
	if slPct != 0 {
		rr = tpPct / slPct
	}
	base := strings.TrimSuffix(sig.Symbol, "USDT")

	// Pending key: "rs{dbID}" for userbot signals, nanosecond for manual
	var id string
	if dbID > 0 {
		id = fmt.Sprintf("rs%d", dbID)
	} else {
		id = fmt.Sprintf("%d", time.Now().UnixNano())
	}

	ps := &pendingSignal{
		Symbol:    sig.Symbol,
		Side:      "Buy",
		EntryLow:  sig.EntryLow,
		EntryHigh: sig.EntryHigh,
		EntryMid:  entryMid,
		SL:        sig.SL,
		TP:        tp,
		QtyStr:    qtyStr,
		PricePrec: pricePrec,
		Margin:    margin,
		Leverage:  b.cfg.Leverage,
		Qty:       qty,
		DBID:      dbID,
	}
	b.mu.Lock()
	b.pending[id] = ps
	b.mu.Unlock()

	var tpLines string
	for i, t := range sig.TPs {
		pct := (t - entryMid) / entryMid * 100
		mark := ""
		if i == 2 {
			mark = " 🎯 <b>выход 100%</b>"
		}
		tpLines += fmt.Sprintf("  TP%d: <code>$%g</code> (+%.1f%%)%s\n", i+1, t, pct, mark)
	}

	mode := "LIVE"
	if b.cfg.PaperTrading {
		mode = "PAPER"
	}

	text := fmt.Sprintf(
		"🚨 <b>Новый сигнал #%d — %s/USDT %s</b> <i>[%s · ExitTP3]</i>\n\n"+
			"Вход: <code>$%g – $%g</code> (mid: <code>$%s</code>)\n"+
			"Стоп: <code>$%g</code> (−%.1f%%)\n"+
			"R:R:  <code>%.1f:1</code>\n\n"+
			"%s\n"+
			"💰 <b>Позиция</b> (%.0f%% от $%.2f, плечо %.0fx):\n"+
			"Количество: <code>%s %s</code>  Маржа: <code>$%.2f</code>",
		sig.ID, base, sig.Direction, mode,
		sig.EntryLow, sig.EntryHigh, ff(entryMid, pricePrec),
		sig.SL, slPct,
		rr,
		tpLines,
		b.cfg.RiskPct, equity, b.cfg.Leverage,
		qtyStr, base, margin,
	)

	// Авторежим: исполняем сразу, кнопки не нужны. На паузе — обычный путь
	// с кнопками, чтобы сигнал можно было подтвердить после /resume.
	// Лонги возле локальных вершин BTC авторежим пропускает (исторически
	// токсичная зона — вход в силу перед откатом) и отдаёт решение человеку.
	if allowAuto && b.IsAuto() && !b.IsPaused() {
		if warn := b.btcHighWarn(); warn != "" {
			text += warn
			log.Printf("[bot] auto-skip signal #%d %s: BTC near 10d high", sig.ID, sig.Symbol)
		} else {
			msgID := b.sendGetID(b.chatID, text+"\n\n🤖 <i>Авторежим — исполняю без подтверждения</i>")
			if msgID > 0 {
				log.Printf("[bot] auto-executing signal #%d %s", sig.ID, sig.Symbol)
				b.confirmSignal(&tgMessage{MessageID: msgID, Chat: tgChat{ID: b.chatID}}, id, ps)
				return
			}
			// не узнали message_id — падаем на ручное подтверждение
		}
	}

	kb := inlineKB([][]kbBtn{{{
		Text: "✅ Войти", Data: "confirm:" + id,
	}, {
		Text: "❌ Отмена", Data: "cancel:" + id,
	}}})

	b.send(b.chatID, 0, text, kb)
}

// ── /signals ──────────────────────────────────────────────────────────────────

func (b *Bot) cmdSignals(msg *tgMessage) {
	if b.paper == nil {
		b.send(msg.Chat.ID, 0, "⚠️ Для /signals нужно задать DATABASE_URL в .env", nil)
		return
	}

	ctx := context.Background()

	// Fetch fresh signals from Telegram channel history
	if b.userbot != nil {
		b.send(msg.Chat.ID, 0, "🔄 Читаю историю канала...", nil)
		channelSigs, err := b.userbot.FetchRecent(ctx, 24)
		if err != nil {
			log.Printf("[signals] fetch from channel: %v", err)
		}
		// Save any signals not yet in DB
		for _, sig := range channelSigs {
			if len(sig.TPs) < 3 || sig.EntryLow == 0 || sig.SL == 0 {
				continue
			}
			existing, _ := b.paper.FindSignalByTraderID(ctx, sig.ID)
			if existing == nil {
				if _, err := b.paper.SaveSignal(ctx, sig); err != nil {
					log.Printf("[signals] save: %v", err)
				}
			}
		}
	}

	signals, err := b.paper.RecentSignals(ctx, 24)
	if err != nil {
		b.send(msg.Chat.ID, 0, "❌ "+esc(err.Error()), nil)
		return
	}
	if len(signals) == 0 {
		b.send(msg.Chat.ID, 0, "📭 Сигналов за последние 24ч нет", nil)
		return
	}

	pending, confirmed, rejected := 0, 0, 0
	for _, s := range signals {
		switch s.Status {
		case "pending":
			pending++
		case "confirmed":
			confirmed++
		case "rejected":
			rejected++
		}
	}
	b.send(msg.Chat.ID, 0, fmt.Sprintf(
		"📋 <b>Сигналы за 24ч: %d</b>  (⏳%d / ✅%d / ❌%d)",
		len(signals), pending, confirmed, rejected,
	), nil)

	for _, rs := range signals {
		agoStr := formatAgo(time.Since(rs.ReceivedAt))
		base := strings.TrimSuffix(rs.Symbol, "USDT")

		switch rs.Status {
		case "confirmed":
			b.send(msg.Chat.ID, 0, fmt.Sprintf(
				"✅ <b>#%d %s/USDT %s</b> — %s\nПодтверждён",
				rs.SignalID, base, rs.Direction, agoStr,
			), nil)
		case "rejected":
			b.send(msg.Chat.ID, 0, fmt.Sprintf(
				"❌ <b>#%d %s/USDT %s</b> — %s\nОтклонён",
				rs.SignalID, base, rs.Direction, agoStr,
			), nil)
		case "pending":
			pendingKey := fmt.Sprintf("rs%d", rs.ID)
			b.mu.Lock()
			_, inMemory := b.pending[pendingKey]
			b.mu.Unlock()

			if inMemory {
				b.send(msg.Chat.ID, 0, fmt.Sprintf(
					"⏳ <b>#%d %s/USDT %s</b> — %s\n<i>Ожидает ответа</i>",
					rs.SignalID, base, rs.Direction, agoStr,
				), nil)
			} else {
				parsedSig := &ParsedSignal{
					ID:        rs.SignalID,
					Symbol:    rs.Symbol,
					Direction: rs.Direction,
					EntryLow:  rs.EntryLow,
					EntryHigh: rs.EntryHigh,
					SL:        rs.SL,
					TPs:       rs.TPs,
				}
				b.sendSignalMsg(parsedSig, rs.ID, false)
			}
		}
	}
}

func formatAgo(d time.Duration) string {
	if d < time.Minute {
		return "только что"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dм назад", int(d.Minutes()))
	}
	return fmt.Sprintf("%dч назад", int(d.Hours()))
}

func (b *Bot) cmdResume(msg *tgMessage) {
	b.mu.Lock()
	wasPaused := b.paused
	b.paused = false
	b.pauseReason = ""
	b.mu.Unlock()

	if !wasPaused {
		b.send(msg.Chat.ID, 0, "ℹ️ Бот и так активен", nil)
		return
	}
	b.send(msg.Chat.ID, 0, "▶️ <b>Бот возобновлён</b> — сигналы снова принимаются", nil)
	log.Println("[bot] resumed by user")
}

// ── /cancel ───────────────────────────────────────────────────────────────────

func (b *Bot) cmdCancel(msg *tgMessage) {
	n, err := b.bybit.CancelAll()
	if err != nil {
		b.send(msg.Chat.ID, 0, "❌ "+esc(err.Error()), nil)
		return
	}
	b.mu.Lock()
	b.pending = map[string]*pendingSignal{}
	b.mu.Unlock()

	b.send(msg.Chat.ID, 0,
		fmt.Sprintf("🗑 Отменено ордеров: <b>%d</b>", n), nil)
}

// ── Callback (inline buttons) ─────────────────────────────────────────────────

func (b *Bot) handleCallback(cb *tgCallback) {
	b.answerCB(cb.ID, "")

	action, id, _ := strings.Cut(cb.Data, ":")

	// ── balance toggle — no pending signal needed ─────────────────────────────
	if action == "balance" && id == "toggle_equity" {
		b.mu.Lock()
		b.hideEquity = !b.hideEquity
		hide := b.hideEquity
		b.mu.Unlock()

		wi, err := b.bybit.Balance()
		if err != nil {
			b.editMsg(cb.Message.Chat.ID, cb.Message.MessageID,
				"❌ "+esc(err.Error()), nil)
			return
		}
		text := b.formatBalance(wi, hide)
		kb := b.balanceKB(hide)
		b.editMsg(cb.Message.Chat.ID, cb.Message.MessageID, text, kb)
		return
	}

	// ── dip-механика: закрытие на просадке BTC и перезаход на отбое ──────────
	if strings.HasPrefix(action, "dip") && b.paper == nil {
		return // без БД монитор не работает, кнопки — от старых сообщений
	}
	switch action {
	case "dipclose":
		n, pnl := b.dipCloseAll(context.Background())
		b.editMsg(cb.Message.Chat.ID, cb.Message.MessageID, fmt.Sprintf(
			"📉 <b>Закрыто позиций: %d</b> (нереализ. PnL %+.2f$)\nЖду отбоя BTC — предложу перезайти.", n, pnl), nil)
		return
	case "dipignore":
		b.editMsg(cb.Message.Chat.ID, cb.Message.MessageID, "Ок, позиции не тронуты.", nil)
		return
	case "dipreenter":
		n := b.dipReenterAll(context.Background())
		b.editMsg(cb.Message.Chat.ID, cb.Message.MessageID, fmt.Sprintf(
			"📈 <b>Перевыставлено лимитников: %d</b> по ценам старых входов.", n), nil)
		return
	case "dipdrop":
		parked, _ := b.paper.ParkedSetups(context.Background())
		for _, p := range parked {
			if err := b.paper.SetParkedStatus(context.Background(), p.ID, "dropped"); err != nil {
				log.Printf("[dip] drop %d: %v", p.ID, err)
			}
		}
		b.editMsg(cb.Message.Chat.ID, cb.Message.MessageID, "Ок, перезаход отменён.", nil)
		return
	}

	b.mu.Lock()
	sig, ok := b.pending[id]
	b.mu.Unlock()

	if !ok {
		b.editMsg(cb.Message.Chat.ID, cb.Message.MessageID,
			"⚠️ Сигнал устарел или уже обработан", nil)
		return
	}

	switch action {
	case "confirm":
		b.confirmSignal(cb.Message, id, sig)
	case "cancel":
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		// Update DB status for userbot signals (keys starting with "rs")
		if b.paper != nil && strings.HasPrefix(id, "rs") {
			var dbID int64
			fmt.Sscanf(id[2:], "%d", &dbID)
			if dbID > 0 {
				if err := b.paper.UpdateSignalStatus(context.Background(), dbID, "rejected"); err != nil {
					log.Printf("[signals] update status: %v", err)
				}
			}
		}
		base := strings.TrimSuffix(sig.Symbol, "USDT")
		b.editMsg(cb.Message.Chat.ID, cb.Message.MessageID,
			fmt.Sprintf("❌ <b>Отклонено:</b> %s/USDT", base), nil)
	}
}

// btcHighWarn проверяет положение BTC относительно его 10-дневного максимума.
// Возвращает "" когда авторежиму можно входить, иначе — текст предупреждения
// для сообщения с ручными кнопками. Не смогли проверить — тоже отдаём человеку:
// фильтр защищает деньги, при сомнении решает не автомат.
func (b *Bot) btcHighWarn() string {
	pct := b.cfg.BTCHighSkipPct
	if pct <= 0 {
		return ""
	}
	dist, err := b.btcDistFrom10dHigh()
	if err != nil {
		log.Printf("[bot] btc-high check failed: %v", err)
		return "\n\n⚠️ <i>Авторежим: не смог проверить положение BTC — реши вручную</i>"
	}
	log.Printf("[bot] btc vs 10d high: %.2f%% (порог %.0f%%)", dist, pct)
	if dist > -pct {
		return fmt.Sprintf(
			"\n\n⚠️ <i>Авторежим пропустил вход: BTC в %.1f%% от 10-дневного максимума "+
				"(порог %.0f%%) — зона, где лонги исторически убыточны. Решай вручную.</i>",
			-dist, pct)
	}
	return ""
}

// confirmSignal исполняет pending-сигнал; при успехе убирает его из очереди
// и помечает запись в БД подтверждённой. msg — сообщение, которое будет
// редактироваться по ходу исполнения (с кнопки или из авторежима).
func (b *Bot) confirmSignal(msg *tgMessage, id string, sig *pendingSignal) bool {
	ok := b.executeSignal(msg, sig)
	if ok {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		// Update DB status for userbot signals (keys starting with "rs")
		if b.paper != nil && strings.HasPrefix(id, "rs") {
			var dbID int64
			fmt.Sscanf(id[2:], "%d", &dbID)
			if dbID > 0 {
				if err := b.paper.UpdateSignalStatus(context.Background(), dbID, "confirmed"); err != nil {
					log.Printf("[signals] update status: %v", err)
				}
			}
		}
	}
	return ok
}

func (b *Bot) executeSignal(msg *tgMessage, sig *pendingSignal) bool {
	b.mu.Lock()
	paused := b.paused
	reason := b.pauseReason
	b.mu.Unlock()

	if paused {
		b.editMsg(msg.Chat.ID, msg.MessageID,
			"⏸ <b>Бот на паузе</b>\n\n"+esc(reason)+"\n\nИспользуй /resume для возобновления", nil)
		return false
	}

	if b.cfg.PaperTrading {
		b.executePaper(msg, sig)
		return true
	}

	b.editMsg(msg.Chat.ID, msg.MessageID, "⏳ Выставляю ордер...", nil)

	if err := b.bybit.SetLeverage(sig.Symbol, b.cfg.Leverage); err != nil {
		log.Printf("[bybit] set-leverage %s: %v", sig.Symbol, err)
	}

	priceStr := ff(sig.EntryMid, sig.PricePrec)
	slStr := ff(sig.SL, sig.PricePrec)
	tpStr := ff(sig.TP, sig.PricePrec)
	base := strings.TrimSuffix(sig.Symbol, "USDT")

	result, err := b.bybit.PlaceOrder(
		sig.Symbol, sig.Side, sig.QtyStr, priceStr, slStr, tpStr)
	if err != nil {
		var retryKB json.RawMessage
		if sig.DBID > 0 {
			retryKB = inlineKB([][]kbBtn{{{Text: "🔄 Повторить", Data: "confirm:" + fmt.Sprintf("rs%d", sig.DBID)}}})
		}
		b.editMsg(msg.Chat.ID, msg.MessageID,
			"❌ Ошибка ордера: <code>"+esc(err.Error())+"</code>\n\nСигнал сохранён, можешь повторить попытку.", retryKB)
		return false
	}

	text := fmt.Sprintf(
		"✅ <b>Ордер выставлен</b>\n\n"+
			"%s/USDT LONG  <code>%s @ $%s</code>\n"+
			"SL: <code>$%s</code>  TP: <code>$%s</code>\n\n"+
			"<code>%s</code>",
		base, sig.QtyStr, priceStr, slStr, tpStr,
		esc(result.OrderID),
	)

	log.Printf("[order] placed %s %s @ %s  sl=%s tp=%s  id=%s",
		sig.Symbol, sig.QtyStr, priceStr, slStr, tpStr, result.OrderID)

	// SL/TP прикреплены к самому ордеру (см. PlaceOrder) и применятся к
	// позиции в момент исполнения — отдельный вызов не нужен.
	b.editMsg(msg.Chat.ID, msg.MessageID, text, nil)
	return true
}

func (b *Bot) executePaper(msg *tgMessage, sig *pendingSignal) {
	b.editMsg(msg.Chat.ID, msg.MessageID, "⏳ [PAPER] Открываю позицию...", nil)

	trade, err := b.paper.OpenTrade(context.Background(), sig)
	if err != nil {
		b.editMsg(msg.Chat.ID, msg.MessageID,
			"❌ [PAPER] Ошибка: <code>"+esc(err.Error())+"</code>", nil)
		return
	}

	b.editMsg(msg.Chat.ID, msg.MessageID, formatPaperConfirm(trade), nil)

	// Subscribe monitor to watch this symbol
	if b.monitor != nil {
		b.monitor.Subscribe(trade.Symbol)
	}

	log.Printf("[paper] opened trade %d %s @ %.6g  sl=%.6g tp=%.6g  margin=%.2f",
		trade.ID, trade.Symbol, trade.EntryPrice, trade.SL, trade.TP, trade.Margin)
}

// ── Telegram API helpers ──────────────────────────────────────────────────────

type kbBtn struct {
	Text string
	Data string
}

func inlineKB(rows [][]kbBtn) json.RawMessage {
	type btn struct {
		Text         string `json:"text"`
		CallbackData string `json:"callback_data"`
	}
	type keyboard struct {
		InlineKeyboard [][]btn `json:"inline_keyboard"`
	}
	kb := keyboard{}
	for _, row := range rows {
		var r []btn
		for _, b := range row {
			r = append(r, btn{b.Text, b.Data})
		}
		kb.InlineKeyboard = append(kb.InlineKeyboard, r)
	}
	out, _ := json.Marshal(kb)
	return out
}

func (b *Bot) apiURL(method string) string {
	return "https://api.telegram.org/bot" + b.token + "/" + method
}

func (b *Bot) send(chatID int64, replyTo int, text string, kb json.RawMessage) {
	body := map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	if replyTo != 0 {
		body["reply_to_message_id"] = replyTo
	}
	if kb != nil {
		body["reply_markup"] = kb
	}
	b.tgPost("sendMessage", body)
}

// sendGetID отправляет сообщение и возвращает его message_id (0 при ошибке) —
// нужно авторежиму, чтобы редактировать сообщение по ходу исполнения.
func (b *Bot) sendGetID(chatID int64, text string) int {
	raw := b.tgPost("sendMessage", map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	})
	var res struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	json.Unmarshal(raw, &res)
	return res.Result.MessageID
}

func (b *Bot) editMsg(chatID int64, msgID int, text string, kb json.RawMessage) {
	body := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": msgID,
		"text":       text,
		"parse_mode": "HTML",
	}
	if kb != nil {
		body["reply_markup"] = kb
	}
	b.tgPost("editMessageText", body)
}

func (b *Bot) answerCB(id, text string) {
	b.tgPost("answerCallbackQuery", map[string]interface{}{
		"callback_query_id": id,
		"text":              text,
	})
}

// tgPost выполняет метод Bot API и возвращает сырой ответ (nil при ошибке).
func (b *Bot) tgPost(method string, body interface{}) []byte {
	data, _ := json.Marshal(body)

	// log outgoing (trim long text for readability)
	preview := string(data)
	if len(preview) > 200 {
		preview = preview[:200] + "…"
	}
	log.Printf("[tg] → %s  %s", method, preview)

	resp, err := b.hc.Post(b.apiURL(method), "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("[tg] %s error: %v", method, err)
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		log.Printf("[tg] %s %d: %s", method, resp.StatusCode, raw)
		return nil
	}
	return raw
}

// esc escapes HTML special chars for Telegram HTML parse mode.
func esc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// parg parses a float argument (accepts comma or dot as decimal separator).
func parg(s string) float64 {
	f, _ := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	return f
}
