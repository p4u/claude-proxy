package creds

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ProfileURL is the endpoint CheckToken reads. A var so tests can point it at
// a fake server.
var ProfileURL = "https://api.anthropic.com/api/oauth/profile"

// ErrTokenRejected means Anthropic answered 401 to the access token itself.
var ErrTokenRejected = errors.New("access token rejected by Anthropic")

var checkClient = &http.Client{Timeout: 10 * time.Second}

// CheckToken asks Anthropic's profile endpoint whether accessToken still
// works, with the headers the web sign-in sends. nil means 200 and
// ErrTokenRejected means 401; any other error says nothing about the token.
func CheckToken(ctx context.Context, accessToken string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ProfileURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := checkClient.Do(req)
	if err != nil {
		return fmt.Errorf("profile check: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxTokenBody))
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return ErrTokenRejected
	}
	return fmt.Errorf("profile check: unexpected status %d", resp.StatusCode)
}
