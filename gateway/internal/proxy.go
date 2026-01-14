package internal

import (
	"aether/shared/logger"
	"aether/shared/protocol"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
)

type Proxy struct {
	worker *protocol.WorkerNode
	instance *protocol.FunctionInstance
}

func NewProxy(worker *protocol.WorkerNode, instance *protocol.FunctionInstance) *Proxy {
	return &Proxy{worker: worker, instance: instance}
}

func ProxyRequest(w http.ResponseWriter, r *http.Request, instance *protocol.FunctionInstance) {

	target, _ := url.Parse(fmt.Sprintf("http://%s:%d", instance.HostIP, instance.ProxyPort))
	logger.Debug("proxying request", "target", target)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ServeHTTP(w, r)
}