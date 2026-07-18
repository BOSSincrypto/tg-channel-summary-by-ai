// Package telegram contains shared Bot API transport errors.
package telegram

import "errors"

// ErrTokenRevoked identifies a Telegram Bot API 401 that invalidates the
// application's token, regardless of which API method observed it.
var ErrTokenRevoked = errors.New("bot token revoked")
