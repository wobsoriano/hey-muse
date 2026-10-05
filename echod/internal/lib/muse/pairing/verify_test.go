package pairing_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HuskerMinion/techo5/echod/internal/lib/muse/pairing"
)

func TestTokenCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		code int
		body string
		ok   bool
	}{
		"a computer":          {200, `{"vm_list":[{"vm_ws_url":"wss://vm.example","vm_auth_token":"t"}]}`, true},
		"older url field":     {200, `{"vm_list":[{"vm_url":"wss://vm.example","vm_auth_token":"t"}]}`, true},
		"junk entries first":  {200, `{"vm_list":[7,{"vm_url":3},{"vm_url":"wss://vm.example","vm_auth_token":"t"}]}`, true},
		"no computers":        {200, `{"vm_list":[]}`, false},
		"computer, no token":  {200, `{"vm_list":[{"vm_ws_url":"wss://vm.example"}]}`, false},
		"error in a 200":      {200, `{"error_title":"Nope","vm_list":[{"vm_url":"u","vm_auth_token":"t"}]}`, false},
		"backend error code":  {200, `{"backend_error_code":42,"vm_list":[{"vm_url":"u","vm_auth_token":"t"}]}`, false},
		"rejected":            {401, `{}`, false},
		"not json":            {200, `<html>`, false},
		"not an object":       {200, `[]`, false},
		"vm_list not a list":  {200, `{"vm_list":"x"}`, false},
		"server error":        {503, `{"vm_list":[{"vm_url":"u","vm_auth_token":"t"}]}`, false},
		"empty error is fine": {200, `{"error_title":"","vm_list":[{"vm_url":"u","vm_auth_token":"t"}]}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			var got *http.Request
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			check := pairing.TokenCheck{HTTP: srv.Client(), UserAgent: "echod/test"}
			err := check.Verify(context.Background(), pairing.Result{AccessToken: "device-access", APIURLV2: srv.URL + "/"})
			if (err == nil) != tc.ok {
				t.Fatalf("Verify = %v, want ok %v", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), "device-access") {
				t.Fatalf("the error carries the token: %v", err)
			}
			if got.Method != http.MethodGet || got.URL.Path != "/fetch_vms" {
				t.Errorf("asked %s %s", got.Method, got.URL.Path)
			}
			if got.Header.Get("Authorization") != "Bearer device-access" || got.Header.Get("X-API-Version") != "1.0.0" ||
				got.Header.Get("User-Agent") != "echod/test" {
				t.Errorf("headers %v", got.Header)
			}
		})
	}
}

func TestTokenCheckWhenMuseCannotBeReached(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	client, url := srv.Client(), srv.URL
	srv.Close()
	if err := (pairing.TokenCheck{HTTP: client}).Verify(context.Background(), pairing.Result{APIURLV2: url}); err == nil {
		t.Fatal("Verify passed with nobody listening")
	}
}
