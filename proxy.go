package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

var skipHeaders = [...]string{"Connection", "Proxy-Connection", "User-Agent"}
var cidrBlackListConfig = [...]string{"127.0.0.0/8"}

const defaultListenAddress = ":9090"

func getDurationFromEnv(key string, defaultVal string) time.Duration {
	return toDuration(key, getEnvOrDefault(key, defaultVal))
}

func getEnvOrDefault(key string, defaultVal string) string {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	return val
}

func toDuration(key string, val string) time.Duration {
	duration, err := time.ParseDuration(val)
	if err != nil {
		log.Fatalf("Invalid duration value specified for %s: %s", key, val)
	}
	return duration
}

func main() {
	fmt.Printf("Hello egress proxy\n")
	setupLogging()
	httpListenAddress := os.Getenv("PROXY_HTTP_ADDRESS")
	httpsListenAddress := os.Getenv("PROXY_HTTPS_ADDRESS")
	certFile := os.Getenv("CERT_FILE")
	keyFile := os.Getenv("KEY_FILE")
	if httpsListenAddress != "" && (certFile == "" || keyFile == "") {
		log.Fatal("certFile and keyFile must be specified for HTTPS listener")
	}
	if httpListenAddress == "" && httpsListenAddress == "" {
		httpListenAddress = defaultListenAddress
	}
	httpServer, httpsServer := BuildProxyServer(httpListenAddress, httpsListenAddress)
	wg := &sync.WaitGroup{}
	if httpServer != nil {
		wg.Add(1)
		startHTTPServer(httpListenAddress, httpServer, wg)
	}
	if httpsServer != nil {
		wg.Add(1)
		startTLSServer(httpsListenAddress, certFile, keyFile, httpsServer, wg)
	}
	wg.Wait()
}

func setupLogging() {
	if isTruish(os.Getenv("TRACE")) {
		log.SetLevel(log.TraceLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
}

func startHTTPServer(listenAddress string, server *http.Server, wg *sync.WaitGroup) {
	listener, err := net.Listen("tcp4", listenAddress)
	if err != nil {
		log.Fatalf("Could not start egress proxy HTTP listener: %s\n", err)
	}
	go func() {
		if err := server.Serve(listener); err != http.ErrServerClosed {
			log.Fatalf("Failed to start proxy HTTP server: %s\n", err)
		}
		wg.Done()
	}()
}

func startTLSServer(listenAddress, certFile, keyFile string, server *http.Server, wg *sync.WaitGroup) {
	listener, err := net.Listen("tcp4", listenAddress)
	if err != nil {
		log.Fatalf("Could not start egress proxy HTTPS listener: %s\n", err)
	}
	go func() {
		if err := server.ServeTLS(listener, certFile, keyFile); err != http.ErrServerClosed {
			log.Fatalf("Failed to start proxy HTTPS server: %s\n", err)
		}
		wg.Done()
	}()
}

// BuildProxyServer creates a http.Server instance that is ready to proxy requests
func BuildProxyServer(httpListenAddress string, httpsListenAddress string) (*http.Server, *http.Server) {
	connectionDialTimeout := getDurationFromEnv("CONNECT_TIMEOUT", "10s")
	outboundConnectionLifetime := getDurationFromEnv("CONNECTION_LIFETIME", "60s")
	idleReadTimeout := getDurationFromEnv("IDLE_READ_TIMEOUT", "10s")

	dialer := &net.Dialer{
		Timeout:   connectionDialTimeout,
		DualStack: false,
		KeepAlive: -1,
	}

	cidrBlacklist := getCidrBlacklist()

	dialContext := (&safeDialer{dialer: dialer, cidrBlacklist: cidrBlacklist}).DialContext

	skipCertVerification := isTruish(os.Getenv("UNSAFE_SKIP_CERT_VERIFICATION"))

	tr := &http.Transport{
		Proxy:             nil,
		IdleConnTimeout:   time.Duration(20) * time.Second,
		DisableKeepAlives: true,
		DialContext:       dialContext,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: skipCertVerification},
	}
	addresses := []string{httpListenAddress, httpsListenAddress}
	servers := make([]*http.Server, 2, 2)
	for i, address := range addresses {
		if address != "" {
			handler := &ProxyHTTPHandler{
				roundTripper:               tr,
				dialContext:                dialContext,
				outboundConnectionLifetime: outboundConnectionLifetime,
				idleReadTimeout:            idleReadTimeout,
			}
			servers[i] = &http.Server{
				Addr:           address,
				Handler:        handler,
				ConnState:      handler.connStateCallback,
				MaxHeaderBytes: 1 << 20,
			}
		}
	}
	return servers[0], servers[1]
}

func getCidrBlacklist() []net.IPNet {
	if isTruish(os.Getenv("UNSAFE_SKIP_CIDR_BLACKLIST")) {
		return nil
	}

	var cidrBlacklist []net.IPNet
	for _, cidr := range cidrBlackListConfig {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err == nil {
			cidrBlacklist = append(cidrBlacklist, *ipNet)
		}
	}
	return cidrBlacklist
}

// some struct
type ProxyHTTPHandler struct {
	roundTripper               http.RoundTripper
	dialContext                func(ctx context.Context, network, addr string) (net.Conn, error)
	outboundConnectionLifetime time.Duration
	idleReadTimeout            time.Duration
	currentInboundConns        uint32
}

func (p *ProxyHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
	} else {
		ctx, cancel := context.WithTimeout(context.TODO(), p.outboundConnectionLifetime)
		defer cancel()
		start := time.Now()
		resp, err := p.doProxy(ctx, r)
		var responseCode int
		if err != nil {
			responseCode = handleError(w, err)
		} else {
			responseCode = resp.StatusCode
			// XXX: this doesn't work, it writes the whole repsonse from target into the HTTP body
			//resp.Write(w)
			writeResponseHeaders(w, resp)
			p.writeResponseBody(w, resp, cancel)
		}
		duration := time.Now().Sub(start)
		logRequest(r, responseCode, duration)
	}
}

func (p *ProxyHTTPHandler) connStateCallback(conn net.Conn, connState http.ConnState) {
	// NOTE: Hijacked connections do not transition to closed
	if connState == http.StateNew {
		p.incrementInboundConns()
	} else if connState == http.StateClosed {
		p.decrementInboundConns()
	}
}

func (p *ProxyHTTPHandler) incrementInboundConns() {
	updatedInboundConns := atomic.AddUint32(&p.currentInboundConns, 1)
	log.Tracef("New inbound connection opened; current inbound connections = %d\n", updatedInboundConns)
}

func (p *ProxyHTTPHandler) decrementInboundConns() {
	updatedInboundConns := atomic.AddUint32(&p.currentInboundConns, ^uint32(0))
	log.Tracef("Inbound connection closed; current inbound connections = %d\n", updatedInboundConns)
}

func writeResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for k, values := range resp.Header {
		w.Header().Set(k, values[0])
		for _, v := range values[1:] {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
}

func (p *ProxyHTTPHandler) writeResponseBody(w http.ResponseWriter, resp *http.Response, cancel context.CancelFunc) {
	defer resp.Body.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		log.Error("Connection hijacking not supported, all requests will fail!")
		return
	}
	inboundConn, _, err := hj.Hijack()
	if err != nil {
		log.Errorf("Failed to hijack connection: %s\n", err)
		return
	}
	defer func() {
		// XXX: what happens if Close() fails?
		inboundConn.Close()
		p.decrementInboundConns()
	}()
	// XXX: pick optimal buffer size
	buf := make([]byte, 512)
	timer := time.AfterFunc(p.idleReadTimeout, func() {
		cancel()
	})
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := inboundConn.Write(buf[:n])
			if writeErr != nil {
				log.Warnf("Error writing to inbound socket: %s\n", writeErr)
				break
			}
		}
		if err == io.EOF {
			break
		} else if err == context.Canceled {
			log.Info("Socket idle read time out reached")
			break
		} else if err != nil {
			log.Warnf("error occured reading response: %s\n", err)
			break
		}
		timer.Reset(p.idleReadTimeout)
	}
}

func (p *ProxyHTTPHandler) handleConnect(w http.ResponseWriter, r *http.Request) {
	// TODO: think about what context deadlines to set etc
	outboundConn, err := p.dialContext(context.Background(), "tcp4", r.RequestURI)
	if err != nil {
		handleError(w, err)
		return
	}
	defer outboundConn.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Connection hijacking not supported", http.StatusInternalServerError)
		return
	}
	inboundConn, bufrw, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer inboundConn.Close()
	bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n")
	bufrw.WriteString("Connection: Close\r\n")
	bufrw.WriteString("\r\n")
	bufrw.Flush()

	go rawProxy(inboundConn, outboundConn)
	rawProxy(outboundConn, inboundConn)
}

func rawProxy(inConn net.Conn, outConn net.Conn) {
	defer inConn.Close()
	defer outConn.Close()
	buf := make([]byte, 2048)
	for {
		numRead, err := inConn.Read(buf)
		if numRead > 0 {
			_, writeErr := outConn.Write(buf[:numRead])
			// Write must return a non-nil error if it returns n < len(p)
			if writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p ProxyHTTPHandler) doProxy(ctx context.Context, r *http.Request) (*http.Response, error) {
	if !r.URL.IsAbs() {
		return nil, &proxyError{statusCode: http.StatusBadRequest, message: "Request URI must be absolute"}
	}
	if r.URL.Scheme != "http" {
		return nil, &proxyError{statusCode: http.StatusBadRequest, message: "Scheme must be HTTP"}
	}
	//fmt.Fprintf(w, "Hello Go HTTP")
	var outboundUri = r.RequestURI
	if isTLS(r.Header) {
		outboundUri = strings.Replace(outboundUri, "http", "https", 1)
	}
	outboundRequest, err := http.NewRequestWithContext(ctx, r.Method, outboundUri, r.Body)
	if err != nil {
		return nil, err
	}
	copyHeaders(r.Header, outboundRequest.Header)
	outboundRequest.Header["User-Agent"] = []string{"Webhook Sentry/0.1"}
	return p.roundTripper.RoundTrip(outboundRequest)
}

func handleError(w http.ResponseWriter, err error) int {
	switch v := err.(type) {
	case *proxyError:
		http.Error(w, v.message, int(v.statusCode))
		return int(v.statusCode)
	case net.Error:
		if v.Timeout() {
			http.Error(w, "Request to target timed out", http.StatusBadGateway)
			return http.StatusBadGateway
		} else {
			log.Warnf("Unexpected error while proxying request: %s\n", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return http.StatusInternalServerError
		}
	case x509.CertificateInvalidError, x509.HostnameError:
		http.Error(w, v.Error(), http.StatusBadGateway)
		return http.StatusBadGateway
	default:
		log.Warnf("Unexpected error while proxying request: %s\n", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return http.StatusInternalServerError
	}
}

func logRequest(r *http.Request, responseCode int, responseTime time.Duration) {
	requestLogger := log.WithFields(log.Fields{"client_ip": r.RemoteAddr, "method": r.Method, "url": r.RequestURI, "response_code": responseCode,
		"response_time": responseTime})
	requestLogger.Infoln()
}

func isTLS(h http.Header) bool {
	tlsHeader, ok := h["X-Whsentry-Tls"]
	if ok {
		for _, val := range tlsHeader {
			if val == "0" || strings.EqualFold(val, "false") {
				return false
			}
		}
		return true
	}
	return false
}

func copyHeaders(inHeader http.Header, outHeader http.Header) {
	for name, values := range inHeader {
		var skipHeader = false
		for _, skipHeaderName := range skipHeaders {
			if name == skipHeaderName {
				skipHeader = true
				break
			}
		}
		if strings.HasPrefix(name, "X-Whsentry") {
			skipHeader = true
		}
		if !skipHeader {
			for _, value := range values {
				outHeader.Add(name, value)
			}
		}
	}
}

type safeDialer struct {
	dialer        *net.Dialer
	cidrBlacklist []net.IPNet
}

func (s *safeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := s.dialer.Resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var chosenIP net.IP = nil
	for _, ip := range ips {
		if strings.Count(ip.IP.String(), ":") < 2 {
			chosenIP = ip.IP
			break
		}
	}
	if chosenIP == nil {
		//return nil, fmt.Errorf("Target %s did not resolve to a valid IPv4 address", addr)
		return nil, &proxyError{statusCode: http.StatusBadRequest, message: fmt.Sprintf("Target %s did not resolve to a valid IPv4 address", addr)}
	}
	if isBlacklisted(s.cidrBlacklist, chosenIP) {
		return nil, &proxyError{statusCode: http.StatusForbidden, message: fmt.Sprintf("Blacklisted IP %s", chosenIP.String())}
	}

	ipPort := net.JoinHostPort(chosenIP.String(), port)
	return s.dialer.DialContext(ctx, "tcp4", ipPort)
}

func isBlacklisted(cidrBlacklist []net.IPNet, ip net.IP) bool {
	if cidrBlacklist == nil {
		return false
	}
	for _, cidr := range cidrBlacklist {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

type proxyError struct {
	statusCode uint
	message    string
}

func (p *proxyError) Error() string {
	return fmt.Sprintf("%s, Status code: %d", p.message, p.statusCode)
}

func isTruish(val string) bool {
	if val == "" {
		return false
	}
	return val == "1" || strings.EqualFold(val, "true")
}
