package crawler

import (
	"errors"
	"net/url"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/yterajima/go-sitemap"
)

// CrawlResult is the result from a single crawling
type CrawlResult struct {
	URL         string        `json:"url"`
	StatusCode  int           `json:"status-code"`
	Time        time.Duration `json:"server-time"`
	LinkingURLs []string      `json:"linking-urls"`
}

// CrawlStats holds crawling related information: status codes, time
// and totals
type CrawlStats struct {
	Total          int
	StatusCodes    map[int]int
	Average200Time time.Duration
	Max200Time     time.Duration
	Non200Urls     []CrawlResult
}

// CrawlConfig holds crawling configuration.
type CrawlConfig struct {
	Throttle   int
	Host       string
	HTTP       HTTPConfig
	Links      CrawlPageLinksConfig
	HTTPGetter ConcurrentHTTPGetter
}

// CrawlPageLinksConfig holds the crawling policy for links
type CrawlPageLinksConfig struct {
	CrawlExternalLinks bool
	CrawlHyperlinks    bool
	CrawlImages        bool
}

// MergeCrawlStats merges two sets of crawling statistics together.
func MergeCrawlStats(statsA, statsB CrawlStats) (stats CrawlStats) {
	stats.StatusCodes = make(map[int]int)
	stats.Total = statsA.Total + statsB.Total

	if statsA.Max200Time > statsB.Max200Time {
		stats.Max200Time = statsA.Max200Time
	} else {
		stats.Max200Time = statsB.Max200Time
	}

	if statsA.StatusCodes != nil {
		for key, value := range statsA.StatusCodes {
			stats.StatusCodes[key] = stats.StatusCodes[key] + value
		}
	}
	if statsB.StatusCodes != nil {
		for key, value := range statsB.StatusCodes {
			stats.StatusCodes[key] = stats.StatusCodes[key] + value
		}
	}

	if statsA.Average200Time != 0 || statsB.Average200Time != 0 {
		total200ns := (statsA.Average200Time.Nanoseconds()*int64(statsA.StatusCodes[200]) +
			statsB.Average200Time.Nanoseconds()*int64(statsB.StatusCodes[200]))
		stats.Average200Time = time.Duration(total200ns/int64(stats.StatusCodes[200])) * time.Nanosecond
	}

	stats.Non200Urls = append(stats.Non200Urls, statsA.Non200Urls...)
	stats.Non200Urls = append(stats.Non200Urls, statsB.Non200Urls...)

	return
}

// GetSitemapUrls returns all URLs found from the sitemap passed as parameter.
// This function will only retrieve URLs in the sitemap pointed, and in
// sitemaps directly listed (i.e. only 1 level deep or less)
func GetSitemapUrls(sitemapURL string) (urls []*url.URL, err error) {
	sitemap, err := sitemap.Get(sitemapURL, nil)

	if err != nil {
		log.Error(err)
		return
	}

	for _, urlEntry := range sitemap.URL {
		newURL, err := url.Parse(urlEntry.Loc)
		if err != nil {
			log.Error(err)
			continue
		}
		urls = append(urls, newURL)
	}

	return
}

// GetSitemapUrlsAsStrings returns all URLs found as string, from in the
// sitemap passed as parameter.
// This function will only retrieve URLs in the sitemap pointed, and in
// sitemaps directly listed (i.e. only 1 level deep or less)
func GetSitemapUrlsAsStrings(sitemapURL string) (urls []string, err error) {
	typedUrls, err := GetSitemapUrls(sitemapURL)
	for _, url := range typedUrls {
		urls = append(urls, url.String())
	}

	return
}

// AsyncCrawl crawls asynchronously URLs from a sitemap and prints related
// information. Throttle is the maximum number of parallel HTTP requests.
// Host overrides the hostname used in the sitemap if provided,
// and user/pass are optional basic auth credentials
func AsyncCrawl(urls []string, config CrawlConfig, quit <-chan struct{}) (stats CrawlStats, err error) {
	if config.Throttle <= 0 {
		log.Warn("Invalid throttle value, defaulting to 1.")
		config.Throttle = 1
	}
	if config.Host != "" {
		urls = RewriteURLHost(urls, config.Host)
	}

	config.HTTP.ParseLinks = config.Links.CrawlExternalLinks || config.Links.CrawlHyperlinks ||
		config.Links.CrawlImages
	results, stats, server200TimeSum := crawlUrls(urls, config, quit)

	if config.HTTP.ParseLinks {
		_, pageLinksStats, linksServer200TimeSum := crawlPageLinks(results, config, quit)
		stats = MergeCrawlStats(stats, pageLinksStats)
		server200TimeSum += linksServer200TimeSum
	}

	total200 := stats.StatusCodes[200]
	if total200 > 0 {
		stats.Average200Time = server200TimeSum / time.Duration(total200)
	}

	if stats.Total == 0 {
		err = errors.New("no URL crawled")
	} else if stats.Total != stats.StatusCodes[200] {
		err = errors.New("some URLs had a different status code than 200")
	}

	return
}

func crawlPageLinks(sourceResults map[string]*HTTPResponse, sourceConfig CrawlConfig, quit <-chan struct{}) (map[string]*HTTPResponse,
	CrawlStats, time.Duration) {
	linkedUrlsSet := make(map[string][]string)
	for _, result := range sourceResults {
		for _, link := range result.Links {
			if (!sourceConfig.Links.CrawlExternalLinks && link.IsExternal) ||
				(!sourceConfig.Links.CrawlHyperlinks && link.Type == Hyperlink) ||
				(!sourceConfig.Links.CrawlImages && link.Type == Image) {
				continue
			}
			// Skip if already present in sourceResults
			if _, ok := sourceResults[link.TargetURL.String()]; ok {
				continue
			}
			linkedUrlsSet[link.TargetURL.String()] = append(linkedUrlsSet[link.TargetURL.String()], result.URL)
		}
	}

	linkedUrls := make([]string, 0, len(linkedUrlsSet))
	for url := range linkedUrlsSet {
		linkedUrls = append(linkedUrls, url)
	}

	// Make exploration non-recursive by not collecting any more links.
	linksConfig := sourceConfig
	linksConfig.HTTP.ParseLinks = false
	linksConfig.Links = CrawlPageLinksConfig{
		CrawlExternalLinks: false,
		CrawlImages:        false,
		CrawlHyperlinks:    false}

	log.Info("Found ", len(linkedUrls), " relevant linked URL(s)")
	linksResults, linksStats, linksServer200TimeSum := crawlUrls(linkedUrls, linksConfig, quit)

	for i, linkResult := range linksStats.Non200Urls {
		linkResult.LinkingURLs = linkedUrlsSet[linkResult.URL]
		linksStats.Non200Urls[i] = linkResult
	}

	return linksResults, linksStats, linksServer200TimeSum
}

func crawlUrls(urls []string, config CrawlConfig, quit <-chan struct{}) (results map[string]*HTTPResponse,
	stats CrawlStats, server200TimeSum time.Duration) {

	results = make(map[string]*HTTPResponse)
	stats.StatusCodes = make(map[int]int)
	resultsChan := config.HTTPGetter.ConcurrentHTTPGet(urls, config.HTTP, config.Throttle, quit)
	for result := range resultsChan {
		populateCrawlStats(result, &stats, &server200TimeSum)
		results[result.URL] = result
	}
	return
}

func populateCrawlStats(result *HTTPResponse, stats *CrawlStats, total200Time *time.Duration) {
	stats.Total++

	statusCode := result.StatusCode
	serverTime := time.Duration(0)
	if result.Result != nil {
		serverTime = result.Result.Total(result.EndTime)
	}

	stats.StatusCodes[statusCode]++

	if statusCode == 200 {
		*total200Time += serverTime

		if serverTime > stats.Max200Time {
			stats.Max200Time = serverTime
		}
	} else {
		stats.Non200Urls = append(stats.Non200Urls, CrawlResult{
			URL:        result.URL,
			Time:       serverTime,
			StatusCode: statusCode,
		})
	}
}
