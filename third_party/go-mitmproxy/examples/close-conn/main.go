package main

import (
	"github.com/lqqyt2423/go-mitmproxy/proxy"
	log "github.com/sirupsen/logrus"
)

//
// When the proxy forwards HTTPS requests, go-mitmproxy by default initiates an SSL connection with the target server first.
// 1. If you do not want to establish a connection with the target server, such as:
//    Generating a response directly in the RequestHeaders or Request Hook and returning it to the client.
// 2. Or if you want the proxy to establish a connect connection with the client first, delaying the connection with the target service until a real HTTPS request is made.
//
// => Then you can refer to the following code
//    set client.UpstreamCert = false in the ClientConnected Hook.

//
// When proxying https requests, go-mitmproxy by default establishes an SSL connection with the target server first
// 1. If you do not establish a connection with the target server, such as:
//    Generating a Response directly in the Requestheaders or Request Hook and returning it to the client
// 2. Or if you want the man-in-the-middle to establish a connect connection with the client first, delaying the connection between the man-in-the-middle and the target service until a real https request is made
//
// => Then you can refer to the following code
//    set client.UpstreamCert = false in the ClientConnected Hook

type CloseConn struct {
	proxy.BaseAddon
}

func (a *CloseConn) ClientConnected(client *proxy.ClientConn) {
	// necessary
	client.UpstreamCert = false
}

func (a *CloseConn) Requestheaders(f *proxy.Flow) {
	// give some response to client
	// then will not request remote server
	f.Response = &proxy.Response{
		StatusCode: 502,
	}
}

func main() {
	opts := &proxy.Options{
		Addr:              ":9080",
		StreamLargeBodies: 1024 * 1024 * 5,
	}

	p, err := proxy.NewProxy(opts)
	if err != nil {
		log.Fatal(err)
	}

	p.AddAddon(&CloseConn{})
	p.AddAddon(&proxy.LogAddon{})

	log.Fatal(p.Start())
}
