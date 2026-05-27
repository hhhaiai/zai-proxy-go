package internal

import (
	"net"
	"net/http"
	"sync"
	"time"
)

var (
	sharedHTTPClient     *http.Client
	sharedHTTPClientOnce sync.Once
)

// GetSharedHTTPClient returns a shared http.Client with connection pooling.
// All upstream requests share this client to maximise concurrency.
func GetSharedHTTPClient() *http.Client {
	sharedHTTPClientOnce.Do(func() {
		sharedHTTPClient = &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   20,
				MaxConnsPerHost:       20,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
			},
		}
	})
	return sharedHTTPClient
}
