package httpclient

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func New(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout
	transport.MaxIdleConnsPerHost = 4
	return &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func ReadBody(response *http.Response, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, errors.New("could not read HTTP response")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("HTTP response exceeds size limit")
	}
	return data, nil
}

func RetryDelay(header http.Header, now time.Time) time.Duration {
	if value := strings.TrimSpace(header.Get("Retry-After")); value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
			if seconds > 86400 {
				seconds = 86400
			}
			return max(time.Second, time.Duration(seconds)*time.Second)
		}
		if at, err := http.ParseTime(value); err == nil {
			return min(24*time.Hour, max(time.Second, at.Sub(now)))
		}
	}
	if epoch, err := strconv.ParseInt(header.Get("RateLimit-Reset"), 10, 64); err == nil && epoch > 0 {
		return min(24*time.Hour, max(time.Second, time.Unix(epoch, 0).Sub(now)))
	}
	return 0
}
