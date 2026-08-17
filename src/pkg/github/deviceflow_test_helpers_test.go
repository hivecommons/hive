package github

import "net/http"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func withMockDeviceFlow(rt func(*http.Request) (*http.Response, error), fn func()) {
	orig := deviceFlowClient.Transport
	deviceFlowClient.Transport = roundTripFunc(rt)
	defer func() { deviceFlowClient.Transport = orig }()
	fn()
}
