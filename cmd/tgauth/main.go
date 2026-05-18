// tgauth — одноразовый инструмент авторизации userbot-а через QR-код.
// Запусти один раз: go run ./cmd/tgauth/
// Отсканируй QR в Telegram-приложении → Settings → Devices → Link Desktop Device
// Сохранит session_userbot.json, после чего tradebot больше не будет просить авторизацию.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
	"golang.org/x/term"
	"rsc.io/qr"
)

func main() {
	godotenv.Load()

	appID := mustInt("TELEGRAM_APP_ID")
	appHash := mustEnv("TELEGRAM_APP_HASH")

	fmt.Println("=== Telegram userbot QR-авторизация ===")
	fmt.Println()

	dispatcher := tg.NewUpdateDispatcher()
	loggedIn := qrlogin.OnLoginToken(dispatcher)

	client := telegram.NewClient(appID, appHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: "session_userbot.json"},
		UpdateHandler:  dispatcher,
	})

	err := client.Run(context.Background(), func(ctx context.Context) error {
		// Если уже авторизован — просто выводим инфо
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if status.Authorized {
			self, err := client.Self(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("✅ Уже авторизован как %s (@%s)\n", self.FirstName, self.Username)
			fmt.Println("   session_userbot.json актуален — можно запускать tradebot.")
			return nil
		}

		fmt.Println("Открой Telegram на телефоне:")
		fmt.Println("  Settings → Devices → Link Desktop Device")
		fmt.Println("  и отсканируй QR-код ниже.")
		fmt.Println()

		_, err = client.QR().Auth(ctx, loggedIn, func(ctx context.Context, token qrlogin.Token) error {
			printQR(token.URL())
			fmt.Println("Жду сканирования... (QR обновится автоматически если истечёт)")
			return nil
		})
		if errors.Is(err, auth.ErrPasswordAuthNeeded) {
			fmt.Print("\n🔐 Введи пароль 2FA (не отображается): ")
			pwd, pErr := readPassword()
			if pErr != nil {
				return fmt.Errorf("чтение пароля: %w", pErr)
			}
			if _, pErr = client.Auth().Password(ctx, pwd); pErr != nil {
				return fmt.Errorf("неверный пароль 2FA: %w", pErr)
			}
		} else if err != nil {
			return err
		}

		self, err := client.Self(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("\n✅ Авторизован как %s (@%s)\n", self.FirstName, self.Username)
		fmt.Println("   session_userbot.json сохранён — можно запускать tradebot.")
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Ошибка: %v\n", err)
		os.Exit(1)
	}
}

// printQR рендерит QR-код в терминале.
// Каждый модуль QR → один символ: █ (тёмный) или пробел (светлый).
// Два модуля по вертикали → один символ-строку (▀/▄/█/пробел) для компактности.
func printQR(url string) {
	code, err := qr.Encode(url, qr.M)
	if err != nil {
		fmt.Printf("(ошибка генерации QR: %v)\nURL: %s\n\n", err, url)
		return
	}

	size := code.Size
	pad := 2 // белая рамка вокруг

	total := size + pad*2
	border := ""
	for i := 0; i < total; i++ {
		border += "  "
	}

	// Белая полоса сверху
	for i := 0; i < pad; i++ {
		fmt.Println(border)
	}

	// Строки QR (по 2 модуля в высоту → 1 строка символов)
	for y := 0; y < size; y += 2 {
		// Левый отступ
		for i := 0; i < pad; i++ {
			fmt.Print("  ")
		}
		for x := 0; x < size; x++ {
			top := code.Black(x, y)
			bot := y+1 < size && code.Black(x, y+1)
			switch {
			case top && bot:
				fmt.Print("██")
			case top:
				fmt.Print("▀▀")
			case bot:
				fmt.Print("▄▄")
			default:
				fmt.Print("  ")
			}
		}
		// Правый отступ
		for i := 0; i < pad; i++ {
			fmt.Print("  ")
		}
		fmt.Println()
	}

	// Белая полоса снизу
	for i := 0; i < pad; i++ {
		fmt.Println(border)
	}
	fmt.Println()
}

// readPassword читает пароль без эха. Если не терминал — обычный ввод.
func readPassword() (string, error) {
	if term.IsTerminal(int(syscall.Stdin)) {
		b, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Println()
		return string(b), err
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Scan()
	return strings.TrimSpace(sc.Text()), sc.Err()
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "❌ %s не задан в .env\n", key)
		os.Exit(1)
	}
	return v
}

func mustInt(key string) int {
	v := mustEnv(key)
	var n int
	fmt.Sscan(v, &n)
	if n == 0 {
		fmt.Fprintf(os.Stderr, "❌ %s должен быть числом\n", key)
		os.Exit(1)
	}
	return n
}
