package webapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultInitDataMaxAge = 24 * time.Hour
	initDataHeader        = "X-Telegram-Init-Data"
)

// WebAppAuth validates Telegram WebApp initData and restricts access to the
// configured owner. It intentionally keeps initData and bot token private and
// never includes either value in an error or log message.
type WebAppAuth struct {
	botToken string
	ownerID  int64
	maxAge   time.Duration
	now      func() time.Time
}

// NewWebAppAuth creates the authentication boundary used by WebApp APIs.
func NewWebAppAuth(botToken, ownerTelegramID string) (*WebAppAuth, error) {
	if strings.TrimSpace(botToken) == "" {
		return nil, errors.New("WebApp bot token is required")
	}
	ownerID, err := strconv.ParseInt(strings.TrimSpace(ownerTelegramID), 10, 64)
	if err != nil || ownerID <= 0 {
		return nil, errors.New("WebApp owner Telegram ID is invalid")
	}
	return &WebAppAuth{
		botToken: botToken,
		ownerID:  ownerID,
		maxAge:   defaultInitDataMaxAge,
		now:      time.Now,
	}, nil
}

// ValidateInitData validates the Telegram HMAC, auth_date freshness, and
// owner identity. It returns the authenticated Telegram user ID.
func (a *WebAppAuth) ValidateInitData(initData string) (int64, error) {
	if a == nil {
		return 0, errors.New("WebApp authentication is not configured")
	}
	if strings.TrimSpace(initData) == "" {
		return 0, errors.New("missing Telegram WebApp initData")
	}

	values, err := url.ParseQuery(initData)
	if err != nil {
		return 0, errors.New("invalid Telegram WebApp initData")
	}
	providedHash := values.Get("hash")
	if providedHash == "" {
		return 0, errors.New("Telegram WebApp initData hash is missing")
	}

	dataCheckString := makeDataCheckString(values)
	secretKey := hmacDigest([]byte("WebAppData"), []byte(a.botToken))
	expectedHash := hmacDigest(secretKey, []byte(dataCheckString))
	expectedHex := hex.EncodeToString(expectedHash)
	providedBytes, err := hex.DecodeString(providedHash)
	if err != nil || len(providedBytes) != sha256.Size ||
		subtle.ConstantTimeCompare(expectedHash, providedBytes) != 1 ||
		subtle.ConstantTimeCompare([]byte(expectedHex), []byte(strings.ToLower(providedHash))) != 1 {
		return 0, errors.New("invalid Telegram WebApp initData hash")
	}

	authDateValue := values.Get("auth_date")
	authDate, err := strconv.ParseInt(authDateValue, 10, 64)
	if err != nil || authDate <= 0 {
		return 0, errors.New("Telegram WebApp auth_date is invalid")
	}
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	age := now.Sub(time.Unix(authDate, 0))
	if age < -5*time.Minute || age > a.maxAge {
		return 0, errors.New("Telegram WebApp initData expired")
	}

	var user struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(values.Get("user")), &user); err != nil || user.ID <= 0 {
		return 0, errors.New("Telegram WebApp user is invalid")
	}
	if user.ID != a.ownerID {
		return 0, errors.New("Telegram WebApp access denied")
	}
	return user.ID, nil
}

// Middleware requires valid owner initData for a WebApp API request. The
// header is preferred because it avoids putting authentication data in URLs;
// Authorization: tma is supported for clients that cannot set custom headers.
func (a *WebAppAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		initData := r.Header.Get(initDataHeader)
		if initData == "" {
			authorization := r.Header.Get("Authorization")
			if strings.HasPrefix(authorization, "tma ") {
				initData = strings.TrimSpace(strings.TrimPrefix(authorization, "tma "))
			}
		}
		if _, err := a.ValidateInitData(initData); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func makeDataCheckString(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key != "hash" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+strings.Join(values[key], ","))
	}
	return strings.Join(pairs, "\n")
}

func hmacDigest(key, message []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(message)
	return mac.Sum(nil)
}
