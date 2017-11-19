package splash

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"time"

	"github.com/pquerna/cachecontrol/cacheobject"
	"github.com/slotix/dataflowkit/errs"
	"github.com/spf13/viper"
)

var logger *log.Logger

func init() {
	viper.AutomaticEnv() // read in environment variables that match
	logger = log.New(os.Stdout, "splash: ", log.Lshortfile)
}

type Options struct {
	host string //splash host address
	//Splash connection parameters:
	timeout         int
	resourceTimeout int
	wait            float64
}

type Option func(*Options)

func host(h string) Option {
	return func(args *Options) {
		args.host = h
	}
}

func timeout(t int) Option {
	return func(args *Options) {
		args.timeout = t
	}
}

func resourceTimeout(t int) Option {
	return func(args *Options) {
		args.resourceTimeout = t
	}
}

func wait(w float64) Option {
	return func(args *Options) {
		args.wait = w
	}
}

//New creates new connection to Splash Server
func New(req Request, setters ...Option) (splashURL string) {
	//Default options
	args := &Options{
		host:            viper.GetString("SPLASH"),
		timeout:         viper.GetInt("SPLASH_TIMEOUT"),
		resourceTimeout: viper.GetInt("SPLASH_RESOURCE_TIMEOUT"),
		wait:            viper.GetFloat64("SPLASH_WAIT"),
	}
	for _, setter := range setters {
		setter(args)
	}

	//Generating Splash URL
	/*
	   	//"Set-Cookie" from response headers should be sent when accessing for the same domain second time
	   	cookie := `PHPSESSID=ef75e2737a14b06a2749d0b73840354f; path=/; domain=.acer-a500.ru; HttpOnly
	   dle_user_id=deleted; expires=Thu, 01-Jan-1970 00:00:01 GMT; Max-Age=0; path=/; domain=.acer-a500.ru; httponly
	   dle_password=deleted; expires=Thu, 01-Jan-1970 00:00:01 GMT; Max-Age=0; path=/; domain=.acer-a500.ru; httponly
	   dle_hash=deleted; expires=Thu, 01-Jan-1970 00:00:01 GMT; Max-Age=0; path=/; domain=.acer-a500.ru; httponly
	   dle_forum_sessions=ef75e2737a14b06a2749d0b73840354f; expires=Wed, 06-Jun-2018 19:13:00 GMT; Max-Age=31536000; path=/; domain=.acer-a500.ru; httponly
	   forum_last=1496801580; expires=Wed, 06-Jun-2018 19:13:00 GMT; Max-Age=31536000; path=/; domain=.acer-a500.ru; httponly`
	   	//cookie := ""

	   	req.Cookies, err = generateCookie(cookie)
	   	if err != nil {
	   		logger.Println(err)
	   	}
	   	//logger.Println(req.Cookies)

	   	//---------
	*/
	//req.Params = `"auth_key=880ea6a14ea49e853634fbdc5015a024&referer=http%3A%2F%2Fdiesel.elcat.kg%2F&ips_username=dm_&ips_password=asfwwe!444D&rememberMe=1"`

	var LUAScript string
	if IsRobotsTxt(req.URL) {
		LUAScript = robotsLUA
	} else {
		LUAScript = baseLUA
	}
	splashURL = fmt.Sprintf(
		"http://%s/execute?url=%s&timeout=%d&resource_timeout=%d&wait=%.1f&cookies=%s&formdata=%s&lua_source=%s",
		args.host,
		neturl.QueryEscape(req.URL),
		args.timeout,
		args.resourceTimeout,
		args.wait,
		neturl.QueryEscape(req.Cookies),
		neturl.QueryEscape(paramsToLuaTable(req.Params)),
		neturl.QueryEscape(LUAScript))

	return
}

//GetResponse result is passed to storage middleware
//to provide a RFC7234 compliant HTTP cache
func GetResponse(req Request) (*Response, error) {
	splashURL := New(req)
	client := &http.Client{}
	request, err := http.NewRequest("GET", splashURL, nil)
	//req.SetBasicAuth(s.user, s.password)
	resp, err := client.Do(request)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	res, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	//response from Splash service.
	statusCode := resp.StatusCode
	if statusCode != 200 {
		switch statusCode {
		case 504:
			return nil, &errs.GatewayTimeout{}
		default:
			return nil, fmt.Errorf(string(res))
		}
	}

	var sResponse Response

	if err := json.Unmarshal(res, &sResponse); err != nil {
		logger.Println("Json Unmarshall error", err)
	}
	//if response status code is not 200

	if sResponse.Error != "" {
		switch sResponse.Error {
		case "http404":
			return nil, &errs.NotFound{req.URL}
		case "http403":
			return nil, &errs.Forbidden{req.URL}
		case "network3":
			return nil, &errs.InvalidHost{req.URL}
		default:
			return nil, &errs.Error{sResponse.Error}
		}
		//return nil, fmt.Errorf("%s", sResponse.Error)
	}
	// Sometimes no response, request returned  by Splash.
	// gc (garbage collection) method should be called to clear WebKit caches and then
	// GetResponse again. See more at https://github.com/scrapinghub/splash/issues/613

	if sResponse.Response == nil || sResponse.Request == nil && sResponse.HTML != "" {

		var response *Response
		gcResponse, err := gc(viper.GetString("SPLASH"))
		if err == nil && gcResponse.Status == "ok" {
			response, err = GetResponse(req)
			if err != nil {
				return nil, err
			}
		}
		return response, nil
	}

	if !sResponse.Response.Ok {
		if sResponse.Response.Status == 0 {
			err = fmt.Errorf("%s",
				//sResponse.Error)
				sResponse.Response.StatusText)
		} else {
			err = fmt.Errorf("%d. %s",
				sResponse.Response.Status,
				sResponse.Response.StatusText)
		}
		return nil, err
	}
	//if cacheable ?
	sResponse.setCacheInfo()

	return &sResponse, nil

}

func (r *Response) GetContent() (io.ReadCloser, error) {
	//if r == nil {
	//	return nil, errors.New("empty response")
	//}
	if r.Error != "" {
		return nil, errors.New(r.Error)
	}
	if IsRobotsTxt(r.Request.URL) {

		decoded, err := base64.StdEncoding.DecodeString(r.Response.Content.Text)
		if err != nil {
			logger.Println("decode error:", err)
			return nil, err
		}
		readCloser := ioutil.NopCloser(bytes.NewReader(decoded))
		return readCloser, nil
	}

	readCloser := ioutil.NopCloser(strings.NewReader(r.HTML))
	return readCloser, nil
}

//setCacheInfo check if resource is cacheable
//r.Cacheable and r.CacheExpirationTime are filled inside this func
func (r *Response) setCacheInfo() {
	respHeader := r.Response.Headers.(http.Header)
	reqHeader := r.Request.Headers.(http.Header)
	//	respHeader := r.Response.castHeaders()
	//	reqHeader := r.Request.castHeaders()

	reqDir, err := cacheobject.ParseRequestCacheControl(reqHeader.Get("Cache-Control"))
	if err != nil {
		logger.Printf(err.Error())
	}
	resDir, err := cacheobject.ParseResponseCacheControl(respHeader.Get("Cache-Control"))
	if err != nil {
		logger.Printf(err.Error())
	}
	//logger.Println(respHeader)
	expiresHeader, _ := http.ParseTime(respHeader.Get("Expires"))
	dateHeader, _ := http.ParseTime(respHeader.Get("Date"))
	lastModifiedHeader, _ := http.ParseTime(respHeader.Get("Last-Modified"))
	obj := cacheobject.Object{
		//	CacheIsPrivate:         false,
		RespDirectives:         resDir,
		RespHeaders:            respHeader,
		RespStatusCode:         r.Response.Status,
		RespExpiresHeader:      expiresHeader,
		RespDateHeader:         dateHeader,
		RespLastModifiedHeader: lastModifiedHeader,

		ReqDirectives: reqDir,
		ReqHeaders:    reqHeader,
		ReqMethod:     r.Request.Method,
		NowUTC:        time.Now().UTC(),
	}

	rv := cacheobject.ObjectResults{}
	cacheobject.CachableObject(&obj, &rv)
	cacheobject.ExpirationObject(&obj, &rv)

	//Check if it is cacheable

	if len(rv.OutReasons) == 0 {
		r.Cacheable = true
		if rv.OutExpirationTime.IsZero() {
			//if time is zero than set it to current time plus 24 hours.
			r.Expires = time.Now().UTC().Add(time.Hour * 24)
		} else {
			r.Expires = rv.OutExpirationTime
		}
		logger.Println("Current Time: ", time.Now().UTC())
		logger.Println(r.Request.URL, r.Expires)
	} else {
		//if resource is not cacheable set expiration time to the current time.
		//This way web page will be downloaded every time.
		r.Expires = time.Now().UTC()
	}

	debug := false
	if debug {
		if rv.OutErr != nil {
			logger.Println("Errors: ", rv.OutErr)
		}
		if rv.OutReasons != nil {
			logger.Println("Reasons to not cache: ", rv.OutReasons)
		}
		if rv.OutWarnings != nil {
			logger.Println("Warning headers to add: ", rv.OutWarnings)
		}
		logger.Println("Expiration: ", rv.OutExpirationTime.String())
	}
	//return rv
}

//Fetch content from url through Splash server https://github.com/scrapinghub/splash/
func Fetch(req Request) (io.ReadCloser, error) {
	//logger.Println(splashURL)
	response, err := GetResponse(req)
	if err != nil {
		return nil, err
	}
	logger.Println(err)
	content, err := response.GetContent()
	if err == nil {
		return content, nil
	}
	return nil, err
}

//GetURL returns URL from Request
func (r *Request) GetURL() string {
	//trim trailing slash if any.
	//aws s3 bucket item name cannot contain slash at the end.
	return strings.TrimSpace(strings.TrimRight(r.URL, "/"))
}

//http://choly.ca/post/go-json-marshalling/
//UnmarshalJSON convert headers to http.Header type
func (r *Response) UnmarshalJSON(data []byte) error {
	type Alias Response
	aux := &struct {
		Headers interface{} `json:"headers"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	if r.Request != nil {
		r.Request.Headers = castHeaders(r.Request.Headers)
	}
	if r.Response != nil {
		r.Response.Headers = castHeaders(r.Response.Headers)
	}
	return nil
}

//castHeaders serves for casting headers returned by Splash to standard http.Header type
func castHeaders(splashHeaders interface{}) (header http.Header) {
	header = make(map[string][]string)
	switch splashHeaders.(type) {
	case []interface{}:
		for _, h := range splashHeaders.([]interface{}) {
			//var str []string
			str := []string{}
			v, ok := h.(map[string]interface{})["value"].(string)
			if ok {
				str = append(str, v)
				header[h.(map[string]interface{})["name"].(string)] = str
			}
		}
		return header
	case map[string]interface{}:
		for k, v := range splashHeaders.(map[string]interface{}) {
			var str []string
			for _, vv := range v.([]interface{}) {
				str = append(str, vv.(string))
			}
			header[k] = str
		}
		return header
	default:
		return nil
	}
}

//Splash splash:history() may return empty list as it cannot retrieve cached results
//in case request the same page twice. So the only solution for the moment is to call
//http://localhost:8050/_gc if an error occures. It runs the Python garbage collector
//and clears internal WebKit caches. See more at https://github.com/scrapinghub/splash/issues/613
func gc(host string) (*gcResponse, error) {
	client := &http.Client{}
	gcURL := fmt.Sprintf("http://%s/_gc", host)
	req, err := http.NewRequest("POST", gcURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	var gc gcResponse
	err = json.Unmarshal(buf.Bytes(), &gc)
	if err != nil {
		return nil, err
	}
	return &gc, nil
}

//Ping returns status and maxrss from _ping  endpoint
func Ping(host string) (*PingResponse, error) {
	client := &http.Client{}
	pingURL := fmt.Sprintf("http://%s/_ping", host)
	req, err := http.NewRequest("GET", pingURL, nil)
	if err != nil {
		return nil, err
	}
	//req.SetBasicAuth(userName, userPass)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	var p PingResponse
	err = json.Unmarshal(buf.Bytes(), &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}
