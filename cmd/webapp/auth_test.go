package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

// buildInitData constructs a signed initData string the way Telegram does.
func buildInitData(t *testing.T, botToken string, userID int64, authDate time.Time) string {
	t.Helper()
	params := map[string]string{
		"auth_date": fmt.Sprintf("%d", authDate.Unix()),
		"query_id":  "AAtest",
		"user":      fmt.Sprintf(`{"id":%d,"first_name":"Test"}`, userID),
	}
	var pairs []string
	for k, v := range params {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)

	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(botToken))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(strings.Join(pairs, "\n")))
	hash := hex.EncodeToString(mac.Sum(nil))

	v := url.Values{}
	for k, val := range params {
		v.Set(k, val)
	}
	v.Set("hash", hash)
	return v.Encode()
}

func TestValidateInitData(t *testing.T) {
	const token = "12345:test-bot-token"

	id, err := validateInitData(buildInitData(t, token, 309766112, time.Now()), token)
	if err != nil {
		t.Fatalf("valid initData rejected: %v", err)
	}
	if id != 309766112 {
		t.Fatalf("wrong user id: %d", id)
	}

	if _, err := validateInitData(buildInitData(t, "other-token", 309766112, time.Now()), token); err == nil {
		t.Fatal("initData signed with wrong token accepted")
	}

	if _, err := validateInitData(buildInitData(t, token, 309766112, time.Now().Add(-25*time.Hour)), token); err == nil {
		t.Fatal("expired initData accepted")
	}

	if _, err := validateInitData("hash=deadbeef&auth_date=1", token); err == nil {
		t.Fatal("garbage initData accepted")
	}
}
