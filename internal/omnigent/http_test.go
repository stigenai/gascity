package omnigent

import (
	"net/http"
	"net/http/httptest"
)

func newOmnigentHTTPTestServer(handler http.Handler) *httptest.Server {
	return httptest.NewServer(handler)
}
