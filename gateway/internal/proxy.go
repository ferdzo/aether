package internal

import (
	"aether/shared/protocol"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

type Proxy struct {
	worker   *protocol.WorkerNode
	instance *protocol.FunctionInstance
}

func NewProxy(worker *protocol.WorkerNode, instance *protocol.FunctionInstance) *Proxy {
	return &Proxy{worker: worker, instance: instance}
}

func ProxyRequest(w http.ResponseWriter, r *http.Request, instance *protocol.FunctionInstance, funcID string) {
	target, _ := url.Parse(fmt.Sprintf("http://%s:%d", instance.HostIP, instance.ProxyPort))
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{ResponseHeaderTimeout: 30 * time.Second}

	oldPath := r.URL.Path
	prefix := "/functions/" + funcID
	r.URL.Path = strings.TrimPrefix(oldPath, prefix)
	if r.URL.Path == "" {
		r.URL.Path = "/"
	}

	proxy.ServeHTTP(w, r)
}
