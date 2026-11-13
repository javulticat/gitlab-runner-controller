package httpclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRetryDelay(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	for _, test := range []struct {
		value, reset string
		want         time.Duration
	}{
		{"30", "", 30 * time.Second}, {"0", "", time.Second}, {"999999999999999", "", 24 * time.Hour},
		{now.Add(2 * time.Minute).Format(http.TimeFormat), "", 2 * time.Minute},
		{"invalid", "1800000045", 45 * time.Second}, {"-1", "", 0}, {"invalid", "", 0},
	} {
		header := http.Header{"Retry-After": {test.value}, "Ratelimit-Reset": {test.reset}}
		if got := RetryDelay(header, now); got != test.want {
			t.Fatalf("%q %q: got %v, want %v", test.value, test.reset, got, test.want)
		}
	}
}

func TestCredentialsAreNeverRedirected(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	request, err := http.NewRequest(http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("PRIVATE-TOKEN", "secret")
	response, err := New(time.Second).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if called || response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatal("credential-bearing redirect was followed")
	}
}

func TestReadBodyRejectsTruncatedLimits(t *testing.T) {
	response := &http.Response{Body: io.NopCloser(strings.NewReader("12345"))}
	if _, err := ReadBody(response, 4); err == nil {
		t.Fatal("oversized response was accepted")
	}
}
