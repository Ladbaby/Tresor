package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// errSlowDown signals that the device-token endpoint asked the client to
// slow its polling cadence (RFC 8628 "slow_down"). It is not a terminal
// error; pollDevice uses it to increase the poll interval.
var errSlowDown = errors.New("slow_down")

// flexInt accepts a JSON value that may be either a number or a numeric
// string (e.g. "5"). Some providers — ChatGPT in particular — return numeric
// fields such as device-auth "interval" as strings, which a plain int field
// would reject with "cannot unmarshal string into ... type int".
type flexInt int

// UnmarshalJSON decodes either a bare number or a quoted numeric string.
func (f *flexInt) UnmarshalJSON(data []byte) error {
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*f = flexInt(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("flexInt: %q is not a number", s)
		}
		*f = flexInt(n)
		return nil
	}
	return fmt.Errorf("flexInt: cannot parse %s", string(data))
}

// requestDeviceLogin requests a device code from the provider and starts a
// background poller. It populates pl with the user-facing code and polling
// state.
func (m *Manager) requestDeviceLogin(p *Provider, pl *pendingLogin) (*pendingLogin, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	payload := map[string]interface{}{}
	if p.ClientID != "" {
		payload["client_id"] = p.ClientID
	}
	if p.Scopes != "" {
		payload["scope"] = p.Scopes
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.DeviceAuthURL, bytes.NewReader(body))
	if err != nil {
		return pl, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return pl, fmt.Errorf("device auth request: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return pl, err
	}
	if resp.StatusCode != http.StatusOK {
		return pl, fmt.Errorf("device auth endpoint returned %s", resp.Status)
	}

	var dc struct {
		DeviceAuthID string  `json:"device_auth_id"`
		UserCode     string  `json:"user_code"`
		Interval     flexInt `json:"interval"`
		ExpiresIn    flexInt `json:"expires_in"`
		DeviceCode   string  `json:"device_code"`
	}
	if err := json.Unmarshal(rb, &dc); err != nil {
		return pl, fmt.Errorf("parse device auth response: %w", err)
	}
	if dc.UserCode == "" {
		return pl, fmt.Errorf("device auth response missing user_code")
	}
	pl.UserCode = dc.UserCode
	pl.DeviceCode = dc.DeviceCode
	pl.DeviceAuthID = dc.DeviceAuthID
	pl.DevicePollURL = p.DeviceTokenURL
	if dc.Interval < 1 {
		dc.Interval = 5
	}

	// Start the background poller
	pollCtx, pollCancel := context.WithCancel(context.Background())
	pl.pollCancel = pollCancel
	go m.pollDevice(p, pl, int(dc.Interval), pollCtx)
	return pl, nil
}

// pollDevice repeatedly posts to the device-token endpoint until the user
// authorizes (success), the code expires, or the context is cancelled.
// Per RFC 8628, the poll interval is doubled when the server responds
// with "slow_down", capped at 60 seconds.
func (m *Manager) pollDevice(p *Provider, pl *pendingLogin, interval int, ctx context.Context) {
	if interval < 1 {
		interval = int(deviceMinPollInterval.Seconds())
	}
	delay := time.Duration(interval) * time.Second
	deadline := time.Now().Add(devicePollTimeout)

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if time.Now().After(deadline) {
			m.failPending(pl, "device code login timed out — try again")
			return
		}
		if m.pollOnce(p, pl) {
			// Server requested a slower cadence — double the interval (RFC 8628).
			delay *= 2
			if delay > 60*time.Second {
				delay = 60 * time.Second
			}
		}
	}
}

// pollOnce performs a single device-token poll. On success it exchanges
// (ChatGPT-style) if an authorization code came back, then saves the token.
// Returns true when the server requested a slower poll cadence (slow_down).
func (m *Manager) pollOnce(p *Provider, pl *pendingLogin) bool {
	tr, err, pendingCode := m.devicePoll(p, pl)
	if err != nil {
		if errors.Is(err, errSlowDown) {
			return true // signal pollDevice to increase delay
		}
		// Only terminal errors fail the login; "authorization_pending" is
		// the normal polling state.
		if isTerminalDeviceError(err) {
			m.failPending(pl, err.Error())
		}
		return false
	}
	if tr == nil && !pendingCode {
		// Still waiting (authorization_pending) — keep polling.
		return false
	}
	if pendingCode {
		// ChatGPT flow: the poll response carries an authorization_code and
		// code_verifier; exchange them at the token URL.
		form := deviceExchangeForm(p, pl, tr)
		tr, err = m.postToken(p, form)
		if err != nil {
			m.failPending(pl, err.Error())
			return false
		}
	}

	tok := m.buildTokenRow(pl.DownstreamID, p, tr)
	if err := m.store.SaveOAuthToken(tok); err != nil {
		m.failPending(pl, err.Error())
		return false
	}
	pl.Status = "success"
	m.completePending(pl)
	return false
}

// devicePoll posts to the device-token URL. Returns the parsed token
// response when the user has authorized; a deviceErr describing the state
// otherwise. pendingCode is true when a code exchange is still required.
func (m *Manager) devicePoll(p *Provider, pl *pendingLogin) (*tokenResponse, error, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	payload := map[string]interface{}{}
	if pl.DeviceAuthID != "" {
		payload["device_auth_id"] = pl.DeviceAuthID
	}
	if pl.DeviceCode != "" {
		payload["device_code"] = pl.DeviceCode
	}
	if pl.UserCode != "" {
		payload["user_code"] = pl.UserCode
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pl.DevicePollURL, bytes.NewReader(body))
	if err != nil {
		return nil, err, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, err, false
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err, false
	}

	var dr struct {
		Error             string  `json:"error"`
		ErrorMessage      string  `json:"error_description"`
		AccessToken       string  `json:"access_token"`
		AuthorizationCode string  `json:"authorization_code"`
		CodeVerifier      string  `json:"code_verifier"`
		ExpiresIn         flexInt `json:"expires_in"`
	}
	if err := json.Unmarshal(rb, &dr); err != nil {
		return nil, fmt.Errorf("parse device poll response: %w", err), false
	}

	switch {
	case dr.AccessToken != "":
		return &tokenResponse{
			AccessToken: dr.AccessToken,
			TokenType:   "Bearer",
			ExpiresIn:   int64(dr.ExpiresIn),
		}, nil, false
	case dr.AuthorizationCode != "":
		// ChatGPT-style: exchange the authorization code afterwards
		pl.AuthorizedCode = dr.AuthorizationCode
		pl.PollVerifier = dr.CodeVerifier
		return &tokenResponse{}, nil, true
	case dr.Error == "authorization_pending":
		return nil, nil, false
	case dr.Error == "slow_down":
		// RFC 8628: signal pollDevice to increase the poll interval.
		return nil, errSlowDown, false
	case dr.Error == "expired_token":
		return nil, fmt.Errorf("device code expired"), false
	default:
		if dr.Error != "" {
			return nil, fmt.Errorf("device poll error: %s %s", dr.Error, dr.ErrorMessage), false
		}
		return nil, nil, false
	}
}

// deviceExchangeForm builds the form to exchange a device-flow
// authorization code (ChatGPT quirk: it uses an authorization_code grant).
func deviceExchangeForm(p *Provider, pl *pendingLogin, tr *tokenResponse) url.Values {
	f := url.Values{}
	f.Set("grant_type", "authorization_code")
	f.Set("code", pl.AuthorizedCode)
	if p.ClientID != "" {
		f.Set("client_id", p.ClientID)
	}
	if verifier := pl.PollVerifier; verifier != "" {
		f.Set("code_verifier", verifier)
	} else if tr != nil && tr.CodeVerifier != "" {
		f.Set("code_verifier", tr.CodeVerifier)
	}
	if p.DeviceExchangeRedirect != "" {
		f.Set("redirect_uri", p.DeviceExchangeRedirect)
	}
	return f
}

// isTerminalDeviceError reports whether a device-poll error should abort the
// login rather than keep polling.
func isTerminalDeviceError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	switch msg {
	case "authorization_pending", "slow_down":
		return false
	}
	return true
}
