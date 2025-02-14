package crawler

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tcnksm/go-httpstat"
)

// HTTPResponse holds information from a GET to a specific URL
type HTTPResponse struct {
	URL        string
	Response   *http.Response
	Result     *httpstat.Result
	StatusCode int
	EndTime    time.Time
	Err        error
	Links      []Link
}

// HTTPConfig holds settings used to get pages via HTTP/S
type HTTPConfig struct {
	User         string
	Pass         string
	Timeout      time.Duration
	ParseLinks   bool
	CustomHeader string // New field: set this to a string in the format "Key: Value"
}

// HTTPGetter performs a single HTTP/S request to the URL and returns information
// related to the result as an HTTPResponse
type HTTPGetter func(client *http.Client, url string, config HTTPConfig) (response *HTTPResponse)

func createRequest(url string) (*http.Request, *httpstat.Result, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Error(err)
		return nil, nil, err
	}

	// create a httpstat powered context
	result := &httpstat.Result{}
	ctx := httpstat.WithHTTPStat(req.Context(), result)
	req = req.WithContext(ctx)

	return req, result, nil
}

func configureRequest(req *http.Request, config HTTPConfig) {
	if len(config.User) > 0 {
		req.SetBasicAuth(config.User, config.Pass)
	}
	if len(config.CustomHeader) > 0 {
		parts := strings.SplitN(config.CustomHeader, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			req.Header.Add(key, value)
		} else {
			log.Warn("Invalid custom header format, expected 'Key: Value'")
		}
	}
}

// HTTPGet issues a GET request to a single URL and returns an HTTPResponse
func HTTPGet(client *http.Client, urlStr string, config HTTPConfig) (response *HTTPResponse) {
	response = &HTTPResponse{
		URL: urlStr,
	}

	req, result, err := createRequest(urlStr)
	if err != nil {
		response.Err = err
		return
	}

	configureRequest(req, config)

	resp, err := client.Do(req)
	response.EndTime = time.Now()
	response.Response = resp
	response.Result = result

	defer func() {
		if resp != nil {
			if !config.ParseLinks {
				io.Copy(io.Discard, resp.Body)
			}
			resp.Body.Close()
		}
		PrintResult(response)
	}()

	if resp == nil {
		response.StatusCode = 0
	} else {
		response.StatusCode = response.Response.StatusCode
	}

	// HTTP client error, won't trigger for 4xx or 5xx
	if err != nil {
		log.Error(err)
		response.Err = err
		return
	}

	if config.ParseLinks {
		currentURL, err := url.Parse(urlStr)
		if err != nil {
			log.Error("error parsing base URL:", err)
			return
		}

		response.Links, err = ExtractLinks(resp.Body, *currentURL)
		if err != nil {
			log.Error("error extracting page links:", err)
			return
		}
	}

	return
}

// ConcurrentHTTPGetter allows concurrent execution of an HTTPGetter
type ConcurrentHTTPGetter interface {
	ConcurrentHTTPGet(urls []string, config HTTPConfig, maxConcurrent int,
		quit <-chan struct{}) <-chan *HTTPResponse
}

// BaseConcurrentHTTPGetter implements HTTPGetter interface using net/http package
type BaseConcurrentHTTPGetter struct {
	Get HTTPGetter
}

// ConcurrentHTTPGet will GET the URLs passed and return the results of the crawling
func (getter *BaseConcurrentHTTPGetter) ConcurrentHTTPGet(urls []string, config HTTPConfig,
	maxConcurrent int, quit <-chan struct{}) <-chan *HTTPResponse {

	resultChan := make(chan *HTTPResponse, len(urls))

	go RunConcurrentGet(getter.Get, urls, config, maxConcurrent, resultChan, quit)

	return resultChan
}

// RunConcurrentGet runs multiple HTTP requests in parallel, and returns the
// results in resultChan
func RunConcurrentGet(httpGet HTTPGetter, urls []string, config HTTPConfig,
	maxConcurrent int, resultChan chan<- *HTTPResponse, quit <-chan struct{}) {

	var wg sync.WaitGroup
	clientsReady := make(chan *http.Client, maxConcurrent)
	for i := 0; i < maxConcurrent; i++ {
		clientsReady <- &http.Client{
			Timeout: config.Timeout,
		}
	}

	defer func() {
		wg.Wait()
		close(resultChan)
	}()

	for _, url := range urls {
		select {
		case <-quit:
			log.Info("Waiting for workers to finish...")
			return
		case client := <-clientsReady:
			wg.Add(1)

			go func(client *http.Client, url string) {
				defer func() {
					clientsReady <- client
					wg.Done()
				}()

				resultChan <- httpGet(client, url, config)
			}(client, url)
		}
	}
}

// PrintResult will print information relative to the HTTPResponse
func PrintResult(result *HTTPResponse) {
	total := int(result.Result.Total(result.EndTime).Round(time.Millisecond) / time.Millisecond)

	if log.GetLevel() == log.DebugLevel {
		log.WithFields(log.Fields{
			"status":  result.StatusCode,
			"dns":     int(result.Result.DNSLookup / time.Millisecond),
			"tcpconn": int(result.Result.TCPConnection / time.Millisecond),
			"tls":     int(result.Result.TLSHandshake / time.Millisecond),
			"server":  int(result.Result.ServerProcessing / time.Millisecond),
			"content": int(result.Result.ContentTransfer(result.EndTime) / time.Millisecond),
			"time":    total,
			"close":   result.EndTime,
		}).Debug("url=" + result.URL)
	} else {
		log.WithFields(log.Fields{
			"status":     result.StatusCode,
			"total-time": total,
		}).Info("url=" + result.URL)
	}
}
