package colly

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/htmlquery"
	"github.com/antchfx/xmlquery"
	"github.com/gocolly/colly/v2/debug"
	"github.com/gocolly/colly/v2/storage"
	"github.com/kennygrant/sanitize"
	whatwgUrl "github.com/nlnwa/whatwg-url/url"
	"github.com/temoto/robotstxt"
	"google.golang.org/appengine/urlfetch"
)

type CollectorOption func(*Collector)

type Collector struct {
	UserAgent                string
	Headers                  *http.Header
	MaxDepth                 int
	AllowedDomains           []string
	DisallowedDomains        []string
	DisallowedURLFilters     []*regexp.Regexp
	URLFilters               []*regexp.Regexp
	AllowURLRevisit          bool
	MaxBodySize              int
	CacheDir                 string
	IgnoreRobotsTxt          bool
	Async                    bool
	ParseHTTPErrorResponse   bool
	ID                       uint32
	DetectCharset            bool
	redirectHandler          func(req *http.Request, via []*http.Request) error
	CheckHead                bool
	TraceHTTP                bool
	Context                  context.Context
	MaxRequests              uint32
	store                    storage.Storage
	debugger                 debug.Debugger
	robotsMap                map[string]*robotstxt.RobotsData
	htmlCallbacks            []*htmlCallbackContainer
	xmlCallbacks             []*xmlCallbackContainer
	requestCallbacks         []RequestCallback
	responseCallbacks        []ResponseCallback
	responseHeadersCallbacks []ResponseHeadersCallback
	errorCallbacks           []ErrorCallback
	scrapedCallbacks         []ScrapedCallback
	requestCount             uint32
	responseCount            uint32
	backend                  *httpBackend
	wg                       *sync.WaitGroup
	lock                     *sync.RWMutex
}

type RequestCallback func(*Request)

type ResponseHeadersCallback func(*Response)

type ResponseCallback func(*Response)

type HTMLCallback func(*HTMLElement)

type XMLCallback func(*XMLElement)

type ErrorCallback func(*Response, error)

type ScrapedCallback func(*Response)

type ProxyFunc func(*http.Request) (*url.URL, error)

type AlreadyVisitedError struct {
	Destination *url.URL
}

func (e *AlreadyVisitedError) Error() string {
	return fmt.Sprintf("%q already visited", e.Destination)
}

type htmlCallbackContainer struct {
	Selector string
	Function HTMLCallback
}

type xmlCallbackContainer struct {
	Query    string
	Function XMLCallback
}

type cookieJarSerializer struct {
	store storage.Storage
	lock  *sync.RWMutex
}

var collectorCounter uint32

type key int

const ProxyURLKey key = iota

var (
	ErrForbiddenDomain     = errors.New("Forbidden domain")
	ErrMissingURL          = errors.New("Missing URL")
	ErrMaxDepth            = errors.New("Max depth limit reached")
	ErrForbiddenURL        = errors.New("ForbiddenURL")
	ErrNoURLFiltersMatch   = errors.New("No URLFilters match")
	ErrRobotsTxtBlocked    = errors.New("URL blocked by robots.txt")
	ErrNoCookieJar         = errors.New("Cookie jar is not available")
	ErrNoPattern           = errors.New("No pattern defined in LimitRule")
	ErrEmptyProxyURL       = errors.New("Proxy URL list is empty")
	ErrAbortedAfterHeaders = errors.New("Aborted after receiving response headers")
	ErrQueueFull           = errors.New("Queue MaxSize reached")
	ErrMaxRequests         = errors.New("Max Requests limit reached")
	ErrRetryBodyUnseekable = errors.New("Retry Body Unseekable")
)

var envMap = map[string]func(*Collector, string){
	"ALLOWED_DOMAINS": func(c *Collector, val string) {
		c.AllowedDomains = strings.Split(val, ",")
	},
	"CACHE_DIR": func(c *Collector, val string) {
		c.CacheDir = val
	},
	"DETECT_CHARSET": func(c *Collector, val string) {
		c.DetectCharset = isYesString(val)
	},
	"DISABLE_COOKIES": func(c *Collector, _ string) {
		c.backend.Client.Jar = nil
	},
	"DISALLOWED_DOMAINS": func(c *Collector, val string) {
		c.DisallowedDomains = strings.Split(val, ",")
	},
	"IGNORE_ROBOTSTXT": func(c *Collector, val string) {
		c.IgnoreRobotsTxt = isYesString(val)
	},
	"FOLLOW_REDIRECTS": func(c *Collector, val string) {
		if !isYesString(val) {
			c.redirectHandler = func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			}
		}
	},
	"MAX_BODY_SIZE": func(c *Collector, val string) {
		size, err := strconv.Atoi(val)
		if err == nil {
			c.MaxBodySize = size
		}
	},
	"MAX_DEPTH": func(c *Collector, val string) {
		maxDepth, err := strconv.Atoi(val)
		if err == nil {
			c.MaxDepth = maxDepth
		}
	},
	"MAX_REQUESTS": func(c *Collector, val string) {
		maxRequests, err := strconv.ParseUint(val, 0, 32)
		if err == nil {
			c.MaxRequests = uint32(maxRequests)
		}
	},
	"PARSE_HTTP_ERROR_RESPONSE": func(c *Collector, val string) {
		c.ParseHTTPErrorResponse = isYesString(val)
	},
	"TRACE_HTTP": func(c *Collector, val string) {
		c.TraceHTTP = isYesString(val)
	},
	"USER_AGENT": func(c *Collector, val string) {
		c.UserAgent = val
	},
}

var urlParser = whatwgUrl.NewParser(whatwgUrl.WithPercentEncodeSinglePercentSign())

func NewCollector(options ...CollectorOption) *Collector {
	c := &Collector{}
	c.Init()

	for _, f := range options {
		f(c)
	}

	c.parseSettingsFromEnv()

	return c
}

func UserAgent(ua string) CollectorOption {
	return func(c *Collector) {
		c.UserAgent = ua
	}
}

func Headers(headers map[string]string) CollectorOption {
	return func(c *Collector) {
		customHeaders := make(http.Header)
		for header, value := range headers {
			customHeaders.Add(header, value)
		}
		c.Headers = &customHeaders
	}
}

func MaxDepth(depth int) CollectorOption {
	return func(c *Collector) {
		c.MaxDepth = depth
	}
}

func MaxRequests(max uint32) CollectorOption {
	return func(c *Collector) {
		c.MaxRequests = max
	}
}

func AllowedDomains(domains ...string) CollectorOption {
	return func(c *Collector) {
		c.AllowedDomains = domains
	}
}

func ParseHTTPErrorResponse() CollectorOption {
	return func(c *Collector) {
		c.ParseHTTPErrorResponse = true
	}
}

func DisallowedDomains(domains ...string) CollectorOption {
	return func(c *Collector) {
		c.DisallowedDomains = domains
	}
}

func DisallowedURLFilters(filters ...*regexp.Regexp) CollectorOption {
	return func(c *Collector) {
		c.DisallowedURLFilters = filters
	}
}

func URLFilters(filters ...*regexp.Regexp) CollectorOption {
	return func(c *Collector) {
		c.URLFilters = filters
	}
}

func AllowURLRevisit() CollectorOption {
	return func(c *Collector) {
		c.AllowURLRevisit = true
	}
}

func MaxBodySize(sizeInBytes int) CollectorOption {
	return func(c *Collector) {
		c.MaxBodySize = sizeInBytes
	}
}

func CacheDir(path string) CollectorOption {
	return func(c *Collector) {
		c.CacheDir = path
	}
}

func IgnoreRobotsTxt() CollectorOption {
	return func(c *Collector) {
		c.IgnoreRobotsTxt = true
	}
}

func TraceHTTP() CollectorOption {
	return func(c *Collector) {
		c.TraceHTTP = true
	}
}

func StdlibContext(ctx context.Context) CollectorOption {
	return func(c *Collector) {
		c.Context = ctx
	}
}

func ID(id uint32) CollectorOption {
	return func(c *Collector) {
		c.ID = id
	}
}

func Async(a ...bool) CollectorOption {
	return func(c *Collector) {
		if len(a) > 0 {
			c.Async = a[0]
		} else {
			c.Async = true
		}
	}
}

func DetectCharset() CollectorOption {
	return func(c *Collector) {
		c.DetectCharset = true
	}
}

func Debugger(d debug.Debugger) CollectorOption {
	return func(c *Collector) {
		d.Init()
		c.debugger = d
	}
}

func CheckHead() CollectorOption {
	return func(c *Collector) {
		c.CheckHead = true
	}
}

func (c *Collector) Init() {
	c.UserAgent = "colly - https://github.com/gocolly/colly/v2"
	c.Headers = nil
	c.MaxDepth = 0
	c.MaxRequests = 0
	c.store = &storage.InMemoryStorage{}
	c.store.Init()
	c.MaxBodySize = 10 * 1024 * 1024
	c.backend = &httpBackend{}
	jar, _ := cookiejar.New(nil)
	c.backend.Init(jar)
	c.backend.Client.CheckRedirect = c.checkRedirectFunc()
	c.wg = &sync.WaitGroup{}
	c.lock = &sync.RWMutex{}
	c.robotsMap = make(map[string]*robotstxt.RobotsData)
	c.IgnoreRobotsTxt = true
	c.ID = atomic.AddUint32(&collectorCounter, 1)
	c.TraceHTTP = false
	c.Context = context.Background()
}

func (c *Collector) Appengine(ctx context.Context) {
	client := urlfetch.Client(ctx)
	client.Jar = c.backend.Client.Jar
	client.CheckRedirect = c.backend.Client.CheckRedirect
	client.Timeout = c.backend.Client.Timeout

	c.backend.Client = client
}

func (c *Collector) Visit(URL string) error {
	if c.CheckHead {
		if check := c.scrape(URL, "HEAD", 1, nil, nil, nil, true); check != nil {
			return check
		}
	}
	return c.scrape(URL, "GET", 1, nil, nil, nil, true)
}

func (c *Collector) HasVisited(URL string) (bool, error) {
	return c.checkHasVisited(URL, nil)
}

func (c *Collector) HasPosted(URL string, requestData map[string]string) (bool, error) {
	return c.checkHasVisited(URL, requestData)
}

func (c *Collector) Head(URL string) error {
	return c.scrape(URL, "HEAD", 1, nil, nil, nil, false)
}

func (c *Collector) Post(URL string, requestData map[string]string) error {
	return c.scrape(URL, "POST", 1, createFormReader(requestData), nil, nil, true)
}

func (c *Collector) PostRaw(URL string, requestData []byte) error {
	return c.scrape(URL, "POST", 1, bytes.NewReader(requestData), nil, nil, true)
}

func (c *Collector) PostMultipart(URL string, requestData map[string][]byte) error {
	boundary := randomBoundary()
	hdr := http.Header{}
	hdr.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	hdr.Set("User-Agent", c.UserAgent)
	return c.scrape(URL, "POST", 1, createMultipartReader(boundary, requestData), nil, hdr, true)
}

func (c *Collector) Request(method, URL string, requestData io.Reader, ctx *Context, hdr http.Header) error {
	return c.scrape(URL, method, 1, requestData, ctx, hdr, true)
}

func (c *Collector) SetDebugger(d debug.Debugger) {
	d.Init()
	c.debugger = d
}

func (c *Collector) UnmarshalRequest(r []byte) (*Request, error) {
	req := &serializableRequest{}
	err := json.Unmarshal(r, req)
	if err != nil {
		return nil, err
	}

	u, err := url.Parse(req.URL)
	if err != nil {
		return nil, err
	}

	ctx := NewContext()
	for k, v := range req.Ctx {
		ctx.Put(k, v)
	}

	return &Request{
		Method:    req.Method,
		URL:       u,
		Depth:     req.Depth,
		Body:      bytes.NewReader(req.Body),
		Ctx:       ctx,
		ID:        atomic.AddUint32(&c.requestCount, 1),
		Headers:   &req.Headers,
		collector: c,
	}, nil
}

func (c *Collector) scrape(u, method string, depth int, requestData io.Reader, ctx *Context, hdr http.Header, checkRevisit bool) error {
	parsedWhatwgURL, err := urlParser.Parse(u)
	if err != nil {
		return err
	}
	parsedURL, err := url.Parse(parsedWhatwgURL.Href(false))
	if err != nil {
		return err
	}
	if hdr == nil {
		hdr = http.Header{}
		if c.Headers != nil {
			for k, v := range *c.Headers {
				for _, value := range v {
					hdr.Add(k, value)
				}
			}
		}
	}
	if _, ok := hdr["User-Agent"]; !ok {
		hdr.Set("User-Agent", c.UserAgent)
	}
	if seeker, ok := requestData.(io.ReadSeeker); ok {
		_, err := seeker.Seek(0, io.SeekStart)
		if err != nil {
			return err
		}
	}

	req, err := http.NewRequest(method, parsedURL.String(), requestData)
	if err != nil {
		return err
	}
	req.Header = hdr
	if hostHeader := hdr.Get("Host"); hostHeader != "" {
		req.Host = hostHeader
	}
	req = req.WithContext(c.Context)
	if err := c.requestCheck(parsedURL, method, req.GetBody, depth, checkRevisit); err != nil {
		return err
	}
	u = parsedURL.String()
	c.wg.Add(1)
	if c.Async {
		go c.fetch(u, method, depth, requestData, ctx, hdr, req)
		return nil
	}
	return c.fetch(u, method, depth, requestData, ctx, hdr, req)
}

func (c *Collector) fetch(u, method string, depth int, requestData io.Reader, ctx *Context, hdr http.Header, req *http.Request) error {
	defer c.wg.Done()
	if ctx == nil {
		ctx = NewContext()
	}
	request := &Request{
		URL:       req.URL,
		Headers:   &req.Header,
		Host:      req.Host,
		Ctx:       ctx,
		Depth:     depth,
		Method:    method,
		Body:      requestData,
		collector: c,
		ID:        atomic.AddUint32(&c.requestCount, 1),
	}

	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}

	c.handleOnRequest(request)

	if request.abort {
		return nil
	}

	if method == "POST" && req.Header.Get("Content-Type") == "" {
		req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	}

	var hTrace *HTTPTrace
	if c.TraceHTTP {
		hTrace = &HTTPTrace{}
		req = hTrace.WithTrace(req)
	}
	origURL := req.URL
	checkHeadersFunc := func(req *http.Request, statusCode int, headers http.Header) bool {
		if req.URL != origURL {
			request.URL = req.URL
			request.Headers = &req.Header
		}
		c.handleOnResponseHeaders(&Response{Ctx: ctx, Request: request, StatusCode: statusCode, Headers: &headers})
		return !request.abort
	}
	response, err := c.backend.Cache(req, c.MaxBodySize, checkHeadersFunc, c.CacheDir)
	if proxyURL, ok := req.Context().Value(ProxyURLKey).(string); ok {
		request.ProxyURL = proxyURL
	}
	if err := c.handleOnError(response, err, request, ctx); err != nil {
		return err
	}
	atomic.AddUint32(&c.responseCount, 1)
	response.Ctx = ctx
	response.Request = request
	response.Trace = hTrace

	err = response.fixCharset(c.DetectCharset, request.ResponseCharacterEncoding)
	if err != nil {
		return err
	}

	c.handleOnResponse(response)

	err = c.handleOnHTML(response)
	if err != nil {
		c.handleOnError(response, err, request, ctx)
	}

	err = c.handleOnXML(response)
	if err != nil {
		c.handleOnError(response, err, request, ctx)
	}

	c.handleOnScraped(response)

	return err
}

func (c *Collector) requestCheck(parsedURL *url.URL, method string, getBody func() (io.ReadCloser, error), depth int, checkRevisit bool) error {
	u := parsedURL.String()
	if c.MaxDepth > 0 && c.MaxDepth < depth {
		return ErrMaxDepth
	}
	if c.MaxRequests > 0 && c.requestCount >= c.MaxRequests {
		return ErrMaxRequests
	}
	if err := c.checkFilters(u, parsedURL.Hostname()); err != nil {
		return err
	}
	if method != "HEAD" && !c.IgnoreRobotsTxt {
		if err := c.checkRobots(parsedURL); err != nil {
			return err
		}
	}
	if checkRevisit && !c.AllowURLRevisit {
		if method != "GET" && getBody == nil {
			return nil
		}

		var body io.ReadCloser
		if getBody != nil {
			var err error
			body, err = getBody()
			if err != nil {
				return err
			}
			defer body.Close()
		}
		uHash := requestHash(u, body)
		visited, err := c.store.IsVisited(uHash)
		if err != nil {
			return err
		}
		if visited {
			return &AlreadyVisitedError{parsedURL}
		}
		return c.store.Visited(uHash)
	}
	return nil
}

func (c *Collector) checkFilters(URL, domain string) error {
	if len(c.DisallowedURLFilters) > 0 {
		if isMatchingFilter(c.DisallowedURLFilters, []byte(URL)) {
			return ErrForbiddenURL
		}
	}
	if len(c.URLFilters) > 0 {
		if !isMatchingFilter(c.URLFilters, []byte(URL)) {
			return ErrNoURLFiltersMatch
		}
	}
	if !c.isDomainAllowed(domain) {
		return ErrForbiddenDomain
	}
	return nil
}

func (c *Collector) isDomainAllowed(domain string) bool {
	for _, d2 := range c.DisallowedDomains {
		if d2 == domain {
			return false
		}
	}
	if c.AllowedDomains == nil || len(c.AllowedDomains) == 0 {
		return true
	}
	for _, d2 := range c.AllowedDomains {
		if d2 == domain {
			return true
		}
	}
	return false
}

func (c *Collector) checkRobots(u *url.URL) error {
	c.lock.RLock()
	robot, ok := c.robotsMap[u.Host]
	c.lock.RUnlock()

	if !ok {
		resp, err := c.backend.Client.Get(u.Scheme + "://" + u.Host + "/robots.txt")
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		robot, err = robotstxt.FromResponse(resp)
		if err != nil {
			return err
		}
		c.lock.Lock()
		c.robotsMap[u.Host] = robot
		c.lock.Unlock()
	}

	uaGroup := robot.FindGroup(c.UserAgent)
	if uaGroup == nil {
		return nil
	}

	eu := u.EscapedPath()
	if u.RawQuery != "" {
		eu += "?" + u.Query().Encode()
	}
	if !uaGroup.Test(eu) {
		return ErrRobotsTxtBlocked
	}
	return nil
}

func (c *Collector) String() string {
	return fmt.Sprintf(
		"Requests made: %d (%d responses) | Callbacks: OnRequest: %d, OnHTML: %d, OnResponse: %d, OnError: %d",
		atomic.LoadUint32(&c.requestCount),
		atomic.LoadUint32(&c.responseCount),
		len(c.requestCallbacks),
		len(c.htmlCallbacks),
		len(c.responseCallbacks),
		len(c.errorCallbacks),
	)
}

func (c *Collector) Wait() {
	c.wg.Wait()
}

func (c *Collector) OnRequest(f RequestCallback) {
	c.lock.Lock()
	if c.requestCallbacks == nil {
		c.requestCallbacks = make([]RequestCallback, 0, 4)
	}
	c.requestCallbacks = append(c.requestCallbacks, f)
	c.lock.Unlock()
}

func (c *Collector) OnResponseHeaders(f ResponseHeadersCallback) {
	c.lock.Lock()
	c.responseHeadersCallbacks = append(c.responseHeadersCallbacks, f)
	c.lock.Unlock()
}

func (c *Collector) OnResponse(f ResponseCallback) {
	c.lock.Lock()
	if c.responseCallbacks == nil {
		c.responseCallbacks = make([]ResponseCallback, 0, 4)
	}
	c.responseCallbacks = append(c.responseCallbacks, f)
	c.lock.Unlock()
}

func (c *Collector) OnHTML(goquerySelector string, f HTMLCallback) {
	c.lock.Lock()
	if c.htmlCallbacks == nil {
		c.htmlCallbacks = make([]*htmlCallbackContainer, 0, 4)
	}
	c.htmlCallbacks = append(c.htmlCallbacks, &htmlCallbackContainer{
		Selector: goquerySelector,
		Function: f,
	})
	c.lock.Unlock()
}

func (c *Collector) OnXML(xpathQuery string, f XMLCallback) {
	c.lock.Lock()
	if c.xmlCallbacks == nil {
		c.xmlCallbacks = make([]*xmlCallbackContainer, 0, 4)
	}
	c.xmlCallbacks = append(c.xmlCallbacks, &xmlCallbackContainer{
		Query:    xpathQuery,
		Function: f,
	})
	c.lock.Unlock()
}

func (c *Collector) OnHTMLDetach(goquerySelector string) {
	c.lock.Lock()
	deleteIdx := -1
	for i, cc := range c.htmlCallbacks {
		if cc.Selector == goquerySelector {
			deleteIdx = i
			break
		}
	}
	if deleteIdx != -1 {
		c.htmlCallbacks = append(c.htmlCallbacks[:deleteIdx], c.htmlCallbacks[deleteIdx+1:]...)
	}
	c.lock.Unlock()
}

func (c *Collector) OnXMLDetach(xpathQuery string) {
	c.lock.Lock()
	deleteIdx := -1
	for i, cc := range c.xmlCallbacks {
		if cc.Query == xpathQuery {
			deleteIdx = i
			break
		}
	}
	if deleteIdx != -1 {
		c.xmlCallbacks = append(c.xmlCallbacks[:deleteIdx], c.xmlCallbacks[deleteIdx+1:]...)
	}
	c.lock.Unlock()
}

func (c *Collector) OnError(f ErrorCallback) {
	c.lock.Lock()
	if c.errorCallbacks == nil {
		c.errorCallbacks = make([]ErrorCallback, 0, 4)
	}
	c.errorCallbacks = append(c.errorCallbacks, f)
	c.lock.Unlock()
}

func (c *Collector) OnScraped(f ScrapedCallback) {
	c.lock.Lock()
	if c.scrapedCallbacks == nil {
		c.scrapedCallbacks = make([]ScrapedCallback, 0, 4)
	}
	c.scrapedCallbacks = append(c.scrapedCallbacks, f)
	c.lock.Unlock()
}

func (c *Collector) SetClient(client *http.Client) {
	c.backend.Client = client
}

func (c *Collector) WithTransport(transport http.RoundTripper) {
	c.backend.Client.Transport = transport
}

func (c *Collector) DisableCookies() {
	c.backend.Client.Jar = nil
}

func (c *Collector) SetCookieJar(j http.CookieJar) {
	c.backend.Client.Jar = j
}

func (c *Collector) SetRequestTimeout(timeout time.Duration) {
	c.backend.Client.Timeout = timeout
}

func (c *Collector) SetStorage(s storage.Storage) error {
	if err := s.Init(); err != nil {
		return err
	}
	c.store = s
	c.backend.Client.Jar = createJar(s)
	return nil
}

func (c *Collector) SetProxy(proxyURL string) error {
	proxyParsed, err := url.Parse(proxyURL)
	if err != nil {
		return err
	}

	c.SetProxyFunc(http.ProxyURL(proxyParsed))

	return nil
}

func (c *Collector) SetProxyFunc(p ProxyFunc) {
	t, ok := c.backend.Client.Transport.(*http.Transport)
	if c.backend.Client.Transport != nil && ok {
		t.Proxy = p
		t.DisableKeepAlives = true
	} else {
		c.backend.Client.Transport = &http.Transport{
			Proxy:             p,
			DisableKeepAlives: true,
		}
	}
}

func createEvent(eventType string, requestID, collectorID uint32, kvargs map[string]string) *debug.Event {
	return &debug.Event{
		CollectorID: collectorID,
		RequestID:   requestID,
		Type:        eventType,
		Values:      kvargs,
	}
}

func (c *Collector) handleOnRequest(r *Request) {
	if c.debugger != nil {
		c.debugger.Event(createEvent("request", r.ID, c.ID, map[string]string{
			"url": r.URL.String(),
		}))
	}
	for _, f := range c.requestCallbacks {
		f(r)
	}
}

func (c *Collector) handleOnResponse(r *Response) {
	if c.debugger != nil {
		c.debugger.Event(createEvent("response", r.Request.ID, c.ID, map[string]string{
			"url":    r.Request.URL.String(),
			"status": http.StatusText(r.StatusCode),
		}))
	}
	for _, f := range c.responseCallbacks {
		f(r)
	}
}

func (c *Collector) handleOnResponseHeaders(r *Response) {
	if c.debugger != nil {
		c.debugger.Event(createEvent("responseHeaders", r.Request.ID, c.ID, map[string]string{
			"url":    r.Request.URL.String(),
			"status": http.StatusText(r.StatusCode),
		}))
	}
	for _, f := range c.responseHeadersCallbacks {
		f(r)
	}
}

func (c *Collector) handleOnHTML(resp *Response) error {
	if len(c.htmlCallbacks) == 0 {
		return nil
	}

	contentType := resp.Headers.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(resp.Body)
	}
	mediatype, _, _ := strings.Cut(contentType, ";")
	mediatype = strings.TrimSpace(strings.ToLower(mediatype))

	switch mediatype {
	case "text/html", "application/xhtml+xml":
	default:
		return nil
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewBuffer(resp.Body))
	if err != nil {
		return err
	}
	if href, found := doc.Find("base[href]").Attr("href"); found {
		u, err := urlParser.ParseRef(resp.Request.URL.String(), href)
		if err == nil {
			baseURL, err := url.Parse(u.Href(false))
			if err == nil {
				resp.Request.baseURL = baseURL
			}
		}

	}
	for _, cc := range c.htmlCallbacks {
		i := 0
		doc.Find(cc.Selector).Each(func(_ int, s *goquery.Selection) {
			for _, n := range s.Nodes {
				e := NewHTMLElementFromSelectionNode(resp, s, n, i)
				i++
				if c.debugger != nil {
					c.debugger.Event(createEvent("html", resp.Request.ID, c.ID, map[string]string{
						"selector": cc.Selector,
						"url":      resp.Request.URL.String(),
					}))
				}
				cc.Function(e)
			}
		})
	}
	return nil
}

func (c *Collector) handleOnXML(resp *Response) error {
	if len(c.xmlCallbacks) == 0 {
		return nil
	}
	contentType := strings.ToLower(resp.Headers.Get("Content-Type"))
	isXMLFile := strings.HasSuffix(strings.ToLower(resp.Request.URL.Path), ".xml") || strings.HasSuffix(strings.ToLower(resp.Request.URL.Path), ".xml.gz")
	if !strings.Contains(contentType, "html") && (!strings.Contains(contentType, "xml") && !isXMLFile) {
		return nil
	}

	if strings.Contains(contentType, "html") {
		doc, err := htmlquery.Parse(bytes.NewBuffer(resp.Body))
		if err != nil {
			return err
		}
		if e := htmlquery.FindOne(doc, "//base"); e != nil {
			for _, a := range e.Attr {
				if a.Key == "href" {
					baseURL, err := resp.Request.URL.Parse(a.Val)
					if err == nil {
						resp.Request.baseURL = baseURL
					}
					break
				}
			}
		}

		for _, cc := range c.xmlCallbacks {
			for _, n := range htmlquery.Find(doc, cc.Query) {
				e := NewXMLElementFromHTMLNode(resp, n)
				if c.debugger != nil {
					c.debugger.Event(createEvent("xml", resp.Request.ID, c.ID, map[string]string{
						"selector": cc.Query,
						"url":      resp.Request.URL.String(),
					}))
				}
				cc.Function(e)
			}
		}
	} else if strings.Contains(contentType, "xml") || isXMLFile {
		doc, err := xmlquery.Parse(bytes.NewBuffer(resp.Body))
		if err != nil {
			return err
		}

		for _, cc := range c.xmlCallbacks {
			xmlquery.FindEach(doc, cc.Query, func(i int, n *xmlquery.Node) {
				e := NewXMLElementFromXMLNode(resp, n)
				if c.debugger != nil {
					c.debugger.Event(createEvent("xml", resp.Request.ID, c.ID, map[string]string{
						"selector": cc.Query,
						"url":      resp.Request.URL.String(),
					}))
				}
				cc.Function(e)
			})
		}
	}
	return nil
}

func (c *Collector) handleOnError(response *Response, err error, request *Request, ctx *Context) error {
	if err == nil && (c.ParseHTTPErrorResponse || response.StatusCode < 203) {
		return nil
	}
	if err == nil && response.StatusCode >= 203 {
		err = errors.New(http.StatusText(response.StatusCode))
	}
	if response == nil {
		response = &Response{
			Request: request,
			Ctx:     ctx,
		}
	}
	if c.debugger != nil {
		c.debugger.Event(createEvent("error", request.ID, c.ID, map[string]string{
			"url":    request.URL.String(),
			"status": http.StatusText(response.StatusCode),
		}))
	}
	if response.Request == nil {
		response.Request = request
	}
	if response.Ctx == nil {
		response.Ctx = request.Ctx
	}
	for _, f := range c.errorCallbacks {
		f(response, err)
	}
	return err
}

func (c *Collector) handleOnScraped(r *Response) {
	if c.debugger != nil {
		c.debugger.Event(createEvent("scraped", r.Request.ID, c.ID, map[string]string{
			"url": r.Request.URL.String(),
		}))
	}
	for _, f := range c.scrapedCallbacks {
		f(r)
	}
}

func (c *Collector) Limit(rule *LimitRule) error {
	return c.backend.Limit(rule)
}

func (c *Collector) Limits(rules []*LimitRule) error {
	return c.backend.Limits(rules)
}

func (c *Collector) SetRedirectHandler(f func(req *http.Request, via []*http.Request) error) {
	c.redirectHandler = f
	c.backend.Client.CheckRedirect = c.checkRedirectFunc()
}

func (c *Collector) SetCookies(URL string, cookies []*http.Cookie) error {
	if c.backend.Client.Jar == nil {
		return ErrNoCookieJar
	}
	u, err := url.Parse(URL)
	if err != nil {
		return err
	}
	c.backend.Client.Jar.SetCookies(u, cookies)
	return nil
}

func (c *Collector) Cookies(URL string) []*http.Cookie {
	if c.backend.Client.Jar == nil {
		return nil
	}
	u, err := url.Parse(URL)
	if err != nil {
		return nil
	}
	return c.backend.Client.Jar.Cookies(u)
}

func (c *Collector) Clone() *Collector {
	return &Collector{
		AllowedDomains:         c.AllowedDomains,
		AllowURLRevisit:        c.AllowURLRevisit,
		CacheDir:               c.CacheDir,
		DetectCharset:          c.DetectCharset,
		DisallowedDomains:      c.DisallowedDomains,
		ID:                     atomic.AddUint32(&collectorCounter, 1),
		IgnoreRobotsTxt:        c.IgnoreRobotsTxt,
		MaxBodySize:            c.MaxBodySize,
		MaxDepth:               c.MaxDepth,
		MaxRequests:            c.MaxRequests,
		DisallowedURLFilters:   c.DisallowedURLFilters,
		URLFilters:             c.URLFilters,
		CheckHead:              c.CheckHead,
		ParseHTTPErrorResponse: c.ParseHTTPErrorResponse,
		UserAgent:              c.UserAgent,
		Headers:                c.Headers,
		TraceHTTP:              c.TraceHTTP,
		Context:                c.Context,
		store:                  c.store,
		backend:                c.backend,
		debugger:               c.debugger,
		Async:                  c.Async,
		redirectHandler:        c.redirectHandler,
		errorCallbacks:         make([]ErrorCallback, 0, 8),
		htmlCallbacks:          make([]*htmlCallbackContainer, 0, 8),
		xmlCallbacks:           make([]*xmlCallbackContainer, 0, 8),
		scrapedCallbacks:       make([]ScrapedCallback, 0, 8),
		lock:                   c.lock,
		requestCallbacks:       make([]RequestCallback, 0, 8),
		responseCallbacks:      make([]ResponseCallback, 0, 8),
		robotsMap:              c.robotsMap,
		wg:                     &sync.WaitGroup{},
	}
}

func (c *Collector) checkRedirectFunc() func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if err := c.checkFilters(req.URL.String(), req.URL.Hostname()); err != nil {
			return fmt.Errorf("Not following redirect to %q: %w", req.URL, err)
		}

		samePageRedirect := normalizeURL(req.URL.String()) == normalizeURL(via[0].URL.String())

		if !c.AllowURLRevisit && !samePageRedirect {
			var body io.ReadCloser
			if req.GetBody != nil {
				var err error
				body, err = req.GetBody()
				if err != nil {
					return err
				}
				defer body.Close()
			}
			uHash := requestHash(req.URL.String(), body)
			visited, err := c.store.IsVisited(uHash)
			if err != nil {
				return err
			}
			if visited {
				return &AlreadyVisitedError{req.URL}
			}
			err = c.store.Visited(uHash)
			if err != nil {
				return err
			}
		}

		if c.redirectHandler != nil {
			return c.redirectHandler(req, via)
		}

		if len(via) >= 10 {
			return http.ErrUseLastResponse
		}

		lastRequest := via[len(via)-1]

		if req.URL.Host != lastRequest.URL.Host {
			req.Header.Del("Authorization")
		}

		return nil
	}
}

func (c *Collector) parseSettingsFromEnv() {
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "COLLY_") {
			continue
		}
		pair := strings.SplitN(e[6:], "=", 2)
		if f, ok := envMap[pair[0]]; ok {
			f(c, pair[1])
		} else {
			log.Println("Unknown environment variable:", pair[0])
		}
	}
}

func (c *Collector) checkHasVisited(URL string, requestData map[string]string) (bool, error) {
	hash := requestHash(URL, createFormReader(requestData))
	return c.store.IsVisited(hash)
}

func SanitizeFileName(fileName string) string {
	ext := filepath.Ext(fileName)
	cleanExt := sanitize.BaseName(ext)
	if cleanExt == "" {
		cleanExt = ".unknown"
	}
	return strings.Replace(fmt.Sprintf(
		"%s.%s",
		sanitize.BaseName(fileName[:len(fileName)-len(ext)]),
		cleanExt[1:],
	), "-", "_", -1)
}

func createFormReader(data map[string]string) io.Reader {
	form := url.Values{}
	for k, v := range data {
		form.Add(k, v)
	}
	return strings.NewReader(form.Encode())
}

func createMultipartReader(boundary string, data map[string][]byte) io.Reader {
	dashBoundary := "--" + boundary

	body := []byte{}
	buffer := bytes.NewBuffer(body)

	buffer.WriteString("Content-type: multipart/form-data; boundary=" + boundary + "\n\n")
	for contentType, content := range data {
		buffer.WriteString(dashBoundary + "\n")
		buffer.WriteString("Content-Disposition: form-data; name=" + contentType + "\n")
		buffer.WriteString(fmt.Sprintf("Content-Length: %d \n\n", len(content)))
		buffer.Write(content)
		buffer.WriteString("\n")
	}
	buffer.WriteString(dashBoundary + "--\n\n")
	return bytes.NewReader(buffer.Bytes())

}

func randomBoundary() string {
	var buf [30]byte
	_, err := io.ReadFull(rand.Reader, buf[:])
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", buf[:])
}

func isYesString(s string) bool {
	switch strings.ToLower(s) {
	case "1", "yes", "true", "y":
		return true
	}
	return false
}

func createJar(s storage.Storage) http.CookieJar {
	return &cookieJarSerializer{store: s, lock: &sync.RWMutex{}}
}

func (j *cookieJarSerializer) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.lock.Lock()
	defer j.lock.Unlock()
	cookieStr := j.store.Cookies(u)
	cnew := make([]*http.Cookie, len(cookies))
	copy(cnew, cookies)
	existing := storage.UnstringifyCookies(cookieStr)
	for _, c := range existing {
		if !storage.ContainsCookie(cnew, c.Name) {
			cnew = append(cnew, c)
		}
	}
	j.store.SetCookies(u, storage.StringifyCookies(cnew))
}

func (j *cookieJarSerializer) Cookies(u *url.URL) []*http.Cookie {
	cookies := storage.UnstringifyCookies(j.store.Cookies(u))
	now := time.Now()
	cnew := make([]*http.Cookie, 0, len(cookies))
	for _, c := range cookies {
		if c.RawExpires != "" && c.Expires.Before(now) {
			continue
		}
		if c.Secure && u.Scheme != "https" {
			continue
		}
		cnew = append(cnew, c)
	}
	return cnew
}

func isMatchingFilter(fs []*regexp.Regexp, d []byte) bool {
	for _, r := range fs {
		if r.Match(d) {
			return true
		}
	}
	return false
}

func normalizeURL(u string) string {
	parsed, err := urlParser.Parse(u)
	if err != nil {
		return u
	}
	return parsed.String()
}

func requestHash(url string, body io.Reader) uint64 {
	h := fnv.New64a()
	io.WriteString(h, normalizeURL(url))
	if body != nil {
		io.Copy(h, body)
	}
	return h.Sum64()
}
