/*
Copyright 2018 The Knative Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package util

import (
	"net/http"
	"strconv"

	"github.com/knative/serving/pkg/activator"

	h2cutil "github.com/knative/serving/pkg/h2c"
	"go.uber.org/zap"
)

type RoundTripperFunc func(*http.Request) (*http.Response, error)

func (rt RoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return rt(r)
}

// NewHttpTransport will use the appropriate transport for the request's HTTP protocol version
func NewHTTPTransport(v1 http.RoundTripper, v2 http.RoundTripper) http.RoundTripper {
	return RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		t := v1
		if r.ProtoMajor == 2 {
			t = v2
		}

		return t.RoundTrip(r)
	})
}

// AutoTransport uses h2c for HTTP2 requests and falls back to `http.DefaultTransport` for all others
var AutoTransport = NewHTTPTransport(http.DefaultTransport, h2cutil.DefaultTransport)

type RetryCond func(*http.Response) bool

// RetryStatus will filter responses matching `status`
func RetryStatus(status int) RetryCond {
	return func(resp *http.Response) bool {
		return resp.StatusCode == status
	}
}

type retryRoundTripper struct {
	logger     *zap.SugaredLogger
	transport  http.RoundTripper
	retryer    Retryer
	retryConds []RetryCond
}

// RetryRoundTripper retries a request on error or retry condition, using the given `retry` strategy
func NewRetryRoundTripper(rt http.RoundTripper, l *zap.SugaredLogger, r Retryer, sr ...RetryCond) http.RoundTripper {
	return &retryRoundTripper{
		logger:     l,
		transport:  rt,
		retryer:    r,
		retryConds: sr,
	}
}

func (rrt *retryRoundTripper) RoundTrip(r *http.Request) (resp *http.Response, err error) {
	// The request body cannot be read multiple times for retries.
	// The workaround is to clone the request body into a byte reader
	// so the body can be read multiple times.
	if r.Body != nil {
		rrt.logger.Debugf("Wrapping body in a rewinder.")
		r.Body = NewRewinder(r.Body)
	}

	attempt := 0
	attempts := rrt.retryer.Retry(func() bool {
		rrt.logger.Debugf("Retrying")

		attempt++
		r.Header.Add(activator.ResponseCountHTTPHeader, strconv.Itoa(attempt))
		resp, err = rrt.transport.RoundTrip(r)

		if err != nil {
			rrt.logger.Errorf("Error making a request: %s", err)
			return true
		}

		for _, retryCond := range rrt.retryConds {
			if retryCond(resp) {
				resp.Body.Close()
				return true
			}
		}

		return false
	})

	if err == nil {
		rrt.logger.Infof("Finished after %d attempt(s). Response code: %d", attempts, resp.StatusCode)

		if resp.Header == nil {
			resp.Header = make(http.Header)
		}

		resp.Header.Add(activator.ResponseCountHTTPHeader, strconv.Itoa(attempts))
	} else {
		rrt.logger.Errorf("Failed after %d attempts. Last error: %v", attempts, err)
	}

	return
}
