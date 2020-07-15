package tests

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
)

func TestProxy_Agent_Disconnect_HTTP_Persistent_Connection(t *testing.T) {
	testcases := []struct {
		name                string
		proxyServerFunction func() (proxy, func(), error)
		clientFunction      func(string, string) (*http.Client, error)
	}{
		{
			name:                "grpc",
			proxyServerFunction: runGRPCProxyServer,
			clientFunction:      createGrpcTunnelClient,
		},
		{
			name:                "http-connect",
			proxyServerFunction: runHTTPConnProxyServer,
			clientFunction:      createHTTPConnectClient,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(newEchoServer("hello"))
			defer server.Close()

			stopCh := make(chan struct{})

			proxy, cleanup, err := tc.proxyServerFunction()
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()

			runAgent(proxy.agent, stopCh)

			// Wait for agent to register on proxy server
			wait.Poll(100*time.Millisecond, 5*time.Second, func() (bool, error) {

				ready, _ := proxy.server.Readiness.Ready()
				return ready, nil
			})

			// run test client

			c, err := tc.clientFunction(proxy.front, server.URL)
			if err != nil {
				t.Errorf("error obtaining client: %v", err)
			}

			_, err = clientRequest(c, server.URL)

			if err != nil {
				t.Errorf("expected no error on proxy request, got %v", err)
			}
			close(stopCh)

			// Wait for the agent to disconnect
			wait.Poll(100*time.Millisecond, 5*time.Second, func() (bool, error) {
				ready, _ := proxy.server.Readiness.Ready()
				return !ready, nil
			})

			// Reuse same client to make the request
			_, err = clientRequest(c, server.URL)
			if err == nil {
				t.Errorf("expect request using http persistent connections to fail after dialing on a broken connection")
			} else if os.IsTimeout(err) {
				t.Errorf("expect request using http persistent connections to fail with error use of closed network connection. Got timeout")
			}
		})
	}
}

func TestProxy_Agent_Reconnect(t *testing.T) {
	testcases := []struct {
		name                string
		proxyServerFunction func() (proxy, func(), error)
		clientFunction      func(string, string) (*http.Client, error)
	}{
		{
			name:                "grpc",
			proxyServerFunction: runGRPCProxyServer,
			clientFunction:      createGrpcTunnelClient,
		},
		{
			name:                "http-connect",
			proxyServerFunction: runHTTPConnProxyServer,
			clientFunction:      createHTTPConnectClient,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {

			server := httptest.NewServer(newEchoServer("hello"))
			defer server.Close()

			stopCh := make(chan struct{})

			proxy, cleanup, err := tc.proxyServerFunction()
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()

			runAgent(proxy.agent, stopCh)

			// Wait for agent to register on proxy server
			wait.Poll(100*time.Millisecond, 5*time.Second, func() (bool, error) {
				ready, _ := proxy.server.Readiness.Ready()
				return ready, nil
			})

			// run test client

			c, err := tc.clientFunction(proxy.front, server.URL)
			if err != nil {
				t.Errorf("error obtaining client: %v", err)
			}

			_, err = clientRequest(c, server.URL)

			if err != nil {
				t.Errorf("expected no error on proxy request, got %v", err)
			}
			close(stopCh)

			// Wait for the agent to disconnect
			wait.Poll(100*time.Millisecond, 5*time.Second, func() (bool, error) {
				ready, _ := proxy.server.Readiness.Ready()
				return !ready, nil
			})

			// Reconnect agent
			stopCh2 := make(chan struct{})
			runAgent(proxy.agent, stopCh2)
			defer close(stopCh2)

			// Wait for agent to register on proxy server
			wait.Poll(100*time.Millisecond, 5*time.Second, func() (bool, error) {
				ready, _ := proxy.server.Readiness.Ready()
				return ready, nil
			})

			// Proxy requests should work again after agent reconnects
			c2, err := tc.clientFunction(proxy.front, server.URL)
			if err != nil {
				t.Errorf("error obtaining client: %v", err)
			}

			_, err = clientRequest(c2, server.URL)

			if err != nil {
				t.Errorf("expected no error on proxy request, got %v", err)
			}
		})
	}
}

func clientRequest(c *http.Client, addr string) ([]byte, error) {
	r, err := c.Get(addr)

	if err != nil {
		return nil, err
	}

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}

	defer r.Body.Close()

	return data, nil
}

func createGrpcTunnelClient(proxyAddr, addr string) (*http.Client, error) {
	tunnel, err := client.CreateSingleUseGrpcTunnel(proxyAddr, grpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	c := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Dial: tunnel.Dial,
		},
	}

	return c, nil
}

func createHTTPConnectClient(proxyAddr, addr string) (*http.Client, error) {
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		return nil, err
	}

	serverURL, _ := url.Parse(addr)

	// Send HTTP-Connect request
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", serverURL.Host, "127.0.0.1")
	if err != nil {
		return nil, err
	}

	// Parse the HTTP response for Connect
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, fmt.Errorf("reading HTTP response from CONNECT: %v", err)
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("expect 200; got %d", res.StatusCode)
	}
	if br.Buffered() > 0 {
		return nil, fmt.Errorf("unexpected extra buffer")
	}

	dialer := func(network, addr string) (net.Conn, error) {
		return conn, nil
	}

	c := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Dial: dialer,
		},
	}

	return c, nil
}
