// This file is a modified port of the token check in linux/src/musegadget/cli.py and muse_api.py from
// Meta's Muse Gadget SDK, Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the Apache
// License, Version 2.0.

package pairing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Result is what setup yields: the device's tokens and where to use them, with the JSON names the SDK
// keeps them under. The SDK also stamps access_token_saved_at when it writes them; that is the
// saver's to add.
type Result struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"` // always "device"
	Username     string `json:"username"`
	APIURL       string `json:"api_url"`    // read only by older firmware; empty unless https
	APIURLV2     string `json:"api_url_v2"` // where API paths go; empty means the Muse API
	NoiseHost    string `json:"noise_host"`
}

// String leaves the tokens out, so a Result that ends up in a log line or an error gives nothing away.
func (r Result) String() string {
	return fmt.Sprintf("pairing.Result{username=%q api_url_v2=%q noise_host=%q}", r.Username, r.APIURLV2, r.NoiseHost)
}

// APIRoot is what API paths are appended to.
func (r Result) APIRoot() string {
	if r.APIURLV2 != "" {
		return strings.TrimRight(r.APIURLV2, "/")
	}
	return "https://" + apiHost
}

const (
	checkTimeout = 15 * time.Second
	maxCheckBody = 1 << 20
)

// TokenCheck asks Muse whether a device token works, the way the SDK does before it saves one: the
// token must list at least one computer the device can reach.
type TokenCheck struct {
	HTTP      *http.Client // nil: http.DefaultClient
	UserAgent string
}

// Verify returns nil when Muse accepts r's access token.
func (c TokenCheck) Verify(ctx context.Context, r Result) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.APIRoot()+"/fetch_vms", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.AccessToken)
	req.Header.Set("X-API-Version", "1.0.0")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("pairing: token check: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("pairing: token check: HTTP %d", resp.StatusCode)
	}
	var body struct {
		ErrorTitle       any               `json:"error_title"`
		BackendErrorCode any               `json:"backend_error_code"`
		VMList           []json.RawMessage `json:"vm_list"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCheckBody)).Decode(&body); err != nil {
		return fmt.Errorf("pairing: token check: %w", err)
	}
	if truthy(body.ErrorTitle) || truthy(body.BackendErrorCode) {
		return fmt.Errorf("pairing: token check: Muse answered %v %v", body.ErrorTitle, body.BackendErrorCode)
	}
	for _, raw := range body.VMList {
		var vm struct {
			WSURL string `json:"vm_ws_url"`
			URL   string `json:"vm_url"`
			Token string `json:"vm_auth_token"`
		}
		if json.Unmarshal(raw, &vm) == nil && (vm.WSURL != "" || vm.URL != "") && vm.Token != "" {
			return nil
		}
	}
	return errors.New("pairing: token check: the token reaches no computer")
}

func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case bool:
		return v
	case float64:
		return v != 0
	}
	return true
}
