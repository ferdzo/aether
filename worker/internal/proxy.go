package internal

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

type Proxy struct {
	proxy *httputil.ReverseProxy
}

func NewProxy(targetURL string) *Proxy {
	target, err := url.Parse(targetURL)
	if err != nil {
		return nil
	}
	return &Proxy{proxy: httputil.NewSingleHostReverseProxy(target)}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.proxy.ServeHTTP(w, r)
}
