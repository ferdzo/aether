package internal

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

type Proxy struct {
	proxy *httputil.ReverseProxy
	instance *Instance
}

func NewProxy(targetURL string, instance *Instance) *Proxy {
	target, err := url.Parse(targetURL)
	if err != nil {
		return nil
	}
	return &Proxy{proxy: httputil.NewSingleHostReverseProxy(target), instance: instance}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.instance.IncrementActiveRequests()
	defer p.instance.DecrementActiveRequests()
	p.proxy.ServeHTTP(w, r)
}
