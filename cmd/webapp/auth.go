package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// validateInitData checks the Telegram Mini App initData signature
// (https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app)
// and returns the authenticated user ID.
func validateInitData(initData, botToken string) (int64, error) {
	values, err := url.ParseQuery(initData)
	if err != nil {
		return 0, fmt.Errorf("parse initData: %w", err)
	}
	gotHash := values.Get("hash")
	if gotHash == "" {
		return 0, fmt.Errorf("no hash in initData")
	}

	var pairs []string
	for k := range values {
		if k == "hash" {
			continue
		}
		pairs = append(pairs, k+"="+values.Get(k))
	}
	sort.Strings(pairs)
	checkString := strings.Join(pairs, "\n")

	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(botToken))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(checkString))
	if hex.EncodeToString(mac.Sum(nil)) != gotHash {
		return 0, fmt.Errorf("bad signature")
	}

	authDate, _ := strconv.ParseInt(values.Get("auth_date"), 10, 64)
	if time.Since(time.Unix(authDate, 0)) > 24*time.Hour {
		return 0, fmt.Errorf("initData expired")
	}

	var user struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(values.Get("user")), &user); err != nil {
		return 0, fmt.Errorf("parse user: %w", err)
	}
	return user.ID, nil
}

// authMiddleware rejects API requests without valid initData from the allowed user.
// The frontend sends initData in the Authorization header: "tma <initData>".
func (s *server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "tma ")
		userID, err := validateInitData(raw, s.botToken)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if userID != s.allowedID {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
