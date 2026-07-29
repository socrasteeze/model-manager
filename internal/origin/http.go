package origin

import (
	"context"
	"errors"
	"io"
	"net/http"
)

func newRequest(ctx context.Context, url, userAgent, apiKey string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req, nil
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, max))
}

func isRateLimit(err error) bool {
	return errors.Is(err, ErrRateLimited)
}
