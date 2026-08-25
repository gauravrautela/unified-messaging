package outlook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/logx"
	"github.com/gauravrautela/unified-messaging/internal/provider"
)

// BaseURL is a var rather than a const so tests can point the client at a
// stand-in Graph server.
var BaseURL = "https://graph.microsoft.com/v1.0"

// Client speaks Microsoft Graph. It implements both provider.Mailbox and
// provider.Pusher; nothing outside this package sees a Graph URL or payload.
type Client struct {
	tokens provider.TokenSource
	http   *http.Client

	// roles caches well-known folder resolution, which costs one request per
	// role and is asked for on every sync round.
	rolesMu    sync.Mutex
	rolesCache map[string]roleEntry
}

type roleEntry struct {
	roles     map[string]string
	expiresAt time.Time
}

func newClient(tokens provider.TokenSource) *Client {
	return &Client{
		tokens: tokens,
		// Generous: Graph occasionally takes many seconds on large delta pages.
		http:       &http.Client{Timeout: 60 * time.Second},
		rolesCache: map[string]roleEntry{},
	}
}

// graphError carries enough of a Graph failure to make a decision about it.
type graphError struct {
	Status  int
	Code    string
	Message string
}

func (e *graphError) Error() string {
	return fmt.Sprintf("outlook: graph %d %s: %s", e.Status, e.Code, e.Message)
}

// codeOf reports the Graph error code carried by err, for logging.
func codeOf(err error) string {
	var ge *graphError
	if errors.As(err, &ge) {
		return ge.Code
	}
	return ""
}

func statusOf(err error) int {
	var ge *graphError
	if errors.As(err, &ge) {
		return ge.Status
	}
	return 0
}

type request struct {
	method string
	// url may be an absolute Graph URL (delta and next links come back fully
	// formed) or a path relative to BaseURL.
	url     string
	body    any
	headers map[string]string
	// out, when non-nil, receives the decoded JSON response.
	out any
	// raw, when non-nil, receives the response body verbatim (attachment bytes).
	raw *[]byte
}

func (c *Client) do(ctx context.Context, accountID string, r request) error {
	const maxAttempts = 4
	forceRefresh := false
	// The Authorization header set below is never logged, at any level.
	log := logx.From(ctx).With("component", "outlook", "account_id", accountID)

	for attempt := 1; ; attempt++ {
		token, err := c.tokens.AccessToken(ctx, accountID, forceRefresh)
		if err != nil {
			log.Debug("graph aborted", "reason", "no access token", "err", err)
			return err
		}

		var bodyReader io.Reader
		if r.body != nil {
			buf, err := json.Marshal(r.body)
			if err != nil {
				return err
			}
			bodyReader = bytes.NewReader(buf)
		}

		u := r.url
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			u = BaseURL + u
		}
		req, err := http.NewRequestWithContext(ctx, r.method, u, bodyReader)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		if r.body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range r.headers {
			req.Header.Set(k, v)
		}

		log.Debug("graph request", "method", r.method, "url", u, "attempt", attempt)
		start := time.Now()
		resp, err := c.http.Do(req)
		if err != nil {
			dur := time.Since(start).Round(time.Millisecond)
			if attempt < maxAttempts {
				log.Debug("graph retry", "reason", "transport error → backoff",
					"attempt", attempt, "wait", backoff(attempt), "dur", dur, "err", err)
				time.Sleep(backoff(attempt))
				continue
			}
			log.Debug("graph response", "status", 0, "dur", dur, "err", err)
			return err
		}

		payload, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		// Bodies are measured, never quoted.
		log.Debug("graph response", "status", resp.StatusCode,
			"bytes", len(payload), "dur", time.Since(start).Round(time.Millisecond))
		if readErr != nil {
			return readErr
		}

		switch {
		case resp.StatusCode == http.StatusUnauthorized && !forceRefresh:
			// The cached token was rejected — usually clock skew or a
			// revocation. Force one refresh before giving up.
			log.Debug("graph retry", "reason", "401 → refresh", "attempt", attempt)
			forceRefresh = true
			continue

		case resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode == http.StatusGatewayTimeout:
			if attempt >= maxAttempts {
				err := classify(resp.StatusCode, payload)
				log.Debug("graph error", "status", resp.StatusCode,
					"graph_code", codeOf(err), "attempts", attempt)
				return err
			}
			// Graph's Retry-After is authoritative; ignoring it is how you earn
			// escalating throttles.
			wait := backoff(attempt)
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil {
					wait = time.Duration(secs) * time.Second
				}
			}
			log.Debug("graph retry", "reason", "429 → backoff", "status", resp.StatusCode,
				"attempt", attempt, "wait", wait, "retry_after", resp.Header.Get("Retry-After"))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue

		case resp.StatusCode >= 400:
			err := classify(resp.StatusCode, payload)
			log.Debug("graph error", "status", resp.StatusCode,
				"graph_code", codeOf(err), "attempts", attempt)
			return err
		}

		if r.raw != nil {
			*r.raw = payload
			return nil
		}
		if r.out != nil && len(payload) > 0 {
			return json.Unmarshal(payload, r.out)
		}
		return nil
	}
}

// classify turns a Graph failure into an error the core can reason about,
// wrapping the shared sentinels where they apply. This is the only place
// Microsoft's error vocabulary is translated.
func classify(status int, body []byte) error {
	var wrapper struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &wrapper)

	msg := wrapper.Error.Message
	if msg == "" {
		msg = string(body)
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
	}
	ge := &graphError{Status: status, Code: wrapper.Error.Code, Message: msg}

	switch {
	// Graph signals a dead delta token two different ways, and both have to be
	// handled or synchronization silently stalls forever.
	case status == http.StatusGone,
		ge.Code == "syncStateNotFound",
		ge.Code == "resyncRequired":
		return fmt.Errorf("%w: %s", provider.ErrCursorExpired, ge)
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %s", provider.ErrNotFound, ge)
	case status == http.StatusConflict:
		return fmt.Errorf("%w: %s", provider.ErrSubscriptionExists, ge)
	}
	return ge
}

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<attempt) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}
