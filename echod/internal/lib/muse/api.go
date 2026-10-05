// Ported from linux/src/musegadget/muse_api.py in Meta's Muse Gadget SDK, Copyright (c) Meta
// Platforms, Inc. and affiliates, licensed under the Apache License, Version 2.0 (LICENSE in this
// directory). Modified: rewritten in Go, and a failure is returned to the caller where the SDK
// logged it.

package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultAPIRoot = "https://api.muse.ai"
	fetchPath      = "/fetch_vms"
	refreshPath    = "/device_token/refresh"
	apiTimeout     = 15 * time.Second
	maxAPIBody     = 256 << 10
)

// vm is one of the owner's Muse VMs, with the bearer that opens a session to it.
type vm struct {
	id        string
	name      string
	authToken string
	isDefault bool
}

type tokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type apiClient struct {
	http      *http.Client
	userAgent string
}

// apiRoot is where the API paths hang. Credentials.APIURL is not consulted: only older firmware
// reads it.
func apiRoot(c *Credentials, allowPlaintext bool) (string, error) {
	root := strings.TrimRight(c.APIURLV2, "/")
	if root == "" {
		return defaultAPIRoot, nil
	}
	if strings.HasPrefix(root, "https://") || (allowPlaintext && strings.HasPrefix(root, "http://")) {
		return root, nil
	}
	// The device token goes to this address, so it does not go anywhere in the clear.
	return "", errors.New("muse: the API address in the credentials is not https")
}

// do returns the status and the body. A status of 0 goes with an error and means no answer came,
// which is worth trying again. Any other status is the server's own word.
func (a *apiClient) do(ctx context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", a.userAgent)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, b, nil
}

// fetchVMs lists the VMs the device token can reach. A 401 means the token was rejected, which
// trying again will not fix.
func (a *apiClient) fetchVMs(ctx context.Context, root, accessToken string) ([]vm, int, error) {
	status, body, err := a.do(ctx, http.MethodGet, root+fetchPath, map[string]string{
		"Authorization": "Bearer " + accessToken,
		"X-API-Version": "1.0.0",
	}, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("muse: VM fetch: %w", err)
	}
	if status != http.StatusOK {
		return nil, status, fmt.Errorf("muse: VM fetch: HTTP %d", status)
	}
	var out struct {
		ErrorTitle       any `json:"error_title"`
		BackendErrorCode any `json:"backend_error_code"`
		VMList           []struct {
			URL       string `json:"vm_url"`
			WSURL     string `json:"vm_ws_url"`
			AuthToken string `json:"vm_auth_token"`
			Name      string `json:"vm_name"`
			ID        string `json:"vm_id"`
			Default   bool   `json:"default"`
		} `json:"vm_list"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, status, fmt.Errorf("muse: VM fetch: not an answer: %w", err)
	}
	if truthy(out.ErrorTitle) || truthy(out.BackendErrorCode) {
		return nil, status, fmt.Errorf("muse: VM fetch: %v %v", out.ErrorTitle, out.BackendErrorCode)
	}
	var vms []vm
	for _, e := range out.VMList {
		if (e.WSURL != "" || e.URL != "") && e.AuthToken != "" {
			vms = append(vms, vm{id: e.ID, name: e.Name, authToken: e.AuthToken, isDefault: e.Default})
		}
	}
	return vms, status, nil
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

// refresh trades the refresh token for a new pair, and the old pair is dead from then on. A 401
// means the pairing is gone and the device must be paired again.
//
// The access token is never presented: the server can accept it and answer 200 with replacement
// tokens that every endpoint then rejects, which would overwrite working credentials.
func (a *apiClient) refresh(ctx context.Context, root, refreshToken, nodeID, sdkToken string) (*tokenPair, int, error) {
	// Apps now hand over refresh tokens that already carry the prefix, and doubling it makes the
	// server reject the token.
	raw := refreshToken[strings.LastIndex(refreshToken, ":")+1:]
	req := map[string]string{"device_id": nodeID}
	if sdkToken != "" {
		req["sdk_token"] = sdkToken
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, 0, err
	}
	status, resp, err := a.do(ctx, http.MethodPost, root+refreshPath, map[string]string{
		"Authorization": "Bearer hatch_refresh:" + raw,
		"Content-Type":  "application/json",
	}, body)
	if err != nil {
		return nil, 0, fmt.Errorf("muse: token refresh: %w", err)
	}
	if status != http.StatusOK {
		return nil, status, fmt.Errorf("muse: token refresh: HTTP %d", status)
	}
	var out struct {
		tokenPair
		Payload *tokenPair `json:"payload"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, 0, fmt.Errorf("muse: token refresh: not an answer: %w", err)
	}
	pair := out.tokenPair
	if out.Payload != nil {
		pair = *out.Payload
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		return nil, status, errors.New("muse: token refresh: the answer has no tokens")
	}
	return &pair, status, nil
}
