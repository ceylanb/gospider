package core

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/extensions"
	"github.com/jaeles-project/gospider/stringset"
	"github.com/spf13/cobra"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var DefaultHTTPTransport = &http.Transport{
	DialContext: (&net.Dialer{
		Timeout: 10 * time.Second,
		// Default is 15 seconds
		KeepAlive: 30 * time.Second,
	}).DialContext,
	MaxIdleConns:    100,
	MaxConnsPerHost: 1000,
	IdleConnTimeout: 30 * time.Second,

	ExpectContinueTimeout: 1 * time.Second,
	ResponseHeaderTimeout: 3 * time.Second,
	DisableCompression:    true,
	TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
}

type Crawler struct {
	cmd                 *cobra.Command
	C                   *colly.Collector
	LinkFinderCollector *colly.Collector
	Output              *Output

	subSet  *stringset.StringFilter
	awsSet  *stringset.StringFilter
	jsSet   *stringset.StringFilter
	urlSet  *stringset.StringFilter
	formSet *stringset.StringFilter

	site   *url.URL
	domain string
}

func NewCrawler(site *url.URL, cmd *cobra.Command) *Crawler {
	domain := GetDomain(site)
	if domain == "" {
		Logger.Error("Failed to parse domain")
		os.Exit(1)
	}
	Logger.Infof("Crawling site: %s", site)

	maxDepth, _ := cmd.Flags().GetInt("depth")
	concurrent, _ := cmd.Flags().GetInt("concurrent")
	delay, _ := cmd.Flags().GetInt("delay")
	randomDelay, _ := cmd.Flags().GetInt("random-delay")

	c := colly.NewCollector(
		colly.Async(true),
		colly.MaxDepth(maxDepth),
		colly.IgnoreRobotsTxt(),
	)

	// Setup http client
	client := &http.Client{}

	// Set proxy
	proxy, _ := cmd.Flags().GetString("proxy")
	if proxy != "" {
		Logger.Info("Proxy: %s", proxy)
		pU, err := url.Parse(proxy)
		if err != nil {
			Logger.Error("Failed to set proxy")
		} else {
			DefaultHTTPTransport.Proxy = http.ProxyURL(pU)
		}
	}

	// Set request timeout
	timeout, _ := cmd.Flags().GetInt("timeout")
	if timeout == 0 {
		Logger.Info("Your input timeout is 0. Gospider will set it to 10 seconds")
		client.Timeout = 10 * time.Second
	} else {
		client.Timeout = time.Duration(timeout) * time.Second
	}

	// Disable redirect
	noRedirect, _ := cmd.Flags().GetBool("no-redirect")
	if noRedirect {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			nextLocation := req.Response.Header.Get("Location")
			Logger.Debugf("Found Redirect: %s", nextLocation)
			// Allow in redirect from http to https or in same hostname
			// We just check contain hostname or not because we set URLFilter in main collector so if
			// the URL is https://otherdomain.com/?url=maindomain.com, it will reject it
			if strings.Contains(nextLocation, site.Hostname()) {
				Logger.Infof("Redirecting to: %s", nextLocation)
				return nil
			}
			return http.ErrUseLastResponse
		}
	}

	// Set client transport
	client.Transport = DefaultHTTPTransport
	c.SetClient(client)

	// Get headers here to overwrite if "burp" flag used
	burpFile, _ := cmd.Flags().GetString("burp")
	if burpFile != "" {
		bF, err := os.Open(burpFile)
		if err != nil {
			Logger.Errorf("Failed to open Burp File: %s", err)
		} else {
			rd := bufio.NewReader(bF)
			req, err := http.ReadRequest(rd)
			if err != nil {
				Logger.Errorf("Failed to Parse Raw Request in %s: %s", burpFile, err)
			} else {
				// Set cookie
				c.OnRequest(func(r *colly.Request) {
					r.Headers.Set("Cookie", GetRawCookie(req.Cookies()))
				})

				// Set headers
				c.OnRequest(func(r *colly.Request) {
					for k, v := range req.Header {
						r.Headers.Set(strings.TrimSpace(k), strings.TrimSpace(v[0]))
					}
				})

			}
		}
	}

	// Set cookies
	cookie, _ := cmd.Flags().GetString("cookie")
	if cookie != "" && burpFile == "" {
		c.OnRequest(func(r *colly.Request) {
			r.Headers.Set("Cookie", cookie)
		})
	}

	// Set headers
	headers, _ := cmd.Flags().GetStringArray("header")
	if burpFile == "" {
		for _, h := range headers {
			headerArgs := strings.SplitN(h, ":", 2)
			headerKey := strings.TrimSpace(headerArgs[0])
			headerValue := strings.TrimSpace(headerArgs[1])
			c.OnRequest(func(r *colly.Request) {
				r.Headers.Set(headerKey, headerValue)
			})
		}
	}

	// Set User-Agent
	randomUA, _ := cmd.Flags().GetString("user-agent")
	switch ua := strings.ToLower(randomUA); {
	case ua == "mobi":
		extensions.RandomMobileUserAgent(c)
	case ua == "web":
		extensions.RandomUserAgent(c)
	default:
		c.UserAgent = ua
	}

	// Set referer
	extensions.Referer(c)

	// Init Output
	var output *Output
	outputFolder, _ := cmd.Flags().GetString("output")
	if outputFolder != "" {
		filename := strings.ReplaceAll(site.Hostname(), ".", "_")
		output = NewOutput(outputFolder, filename)
	}

	// Set url whitelist regex
	sRegex := regexp.MustCompile(`^https?:\/\/(?:[\w\-\_]+\.)+` + domain)
	mRegex := regexp.MustCompile(`^https?:\/\/` + domain)
	c.URLFilters = append(c.URLFilters, sRegex, mRegex)

	// Set Limit Rule
	err := c.Limit(&colly.LimitRule{
		DomainGlob:  domain,
		Parallelism: concurrent,
		Delay:       time.Duration(delay) * time.Second,
		RandomDelay: time.Duration(randomDelay) * time.Second,
	})
	if err != nil {
		Logger.Errorf("Failed to set Limit Rule: %s", err)
		os.Exit(1)
	}

	// GoSpider default disallowed  regex
	disallowedRegex := `(?i).(jpg|jpeg|gif|css|tif|tiff|png|ttf|woff|woff2|ico)(?:\?|#|$)`
	c.DisallowedURLFilters = append(c.DisallowedURLFilters, regexp.MustCompile(disallowedRegex))

	// Set optional blacklist url regex
	blacklists, _ := cmd.Flags().GetString("blacklist")
	if blacklists != "" {
		c.DisallowedURLFilters = append(c.DisallowedURLFilters, regexp.MustCompile(blacklists))
	}

	linkFinderCollector := c.Clone()
	// Try to request as much as Javascript source and don't care about domain.
	// The result of link finder will be send to Link Finder Collector to check is it working or not.
	linkFinderCollector.URLFilters = nil

	return &Crawler{
		cmd:                 cmd,
		C:                   c,
		LinkFinderCollector: linkFinderCollector,
		site:                site,
		domain:              domain,
		Output:              output,
		urlSet:              stringset.NewStringFilter(),
		subSet:              stringset.NewStringFilter(),
		jsSet:               stringset.NewStringFilter(),
		formSet:             stringset.NewStringFilter(),
		awsSet:              stringset.NewStringFilter(),
	}
}

func (crawler *Crawler) Start() {
	// Setup Link Finder
	crawler.setupLinkFinder()

	// Handle url
	crawler.C.OnHTML("[href]", func(e *colly.HTMLElement) {
		urlString := e.Request.AbsoluteURL(e.Attr("href"))
		urlString = FixUrl(urlString, crawler.site)
		if urlString == "" {
			return
		}
		if !crawler.urlSet.Duplicate(urlString) {
			_ = e.Request.Visit(urlString)
		}
	})

	// Handle form
	crawler.C.OnHTML("form[action]", func(e *colly.HTMLElement) {
		formUrl := e.Request.URL.String()
		if !crawler.formSet.Duplicate(formUrl) {
			outputFormat := fmt.Sprintf("[form] - %s", formUrl)
			fmt.Println(outputFormat)
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}

		}
	})

	// Find Upload Form
	uploadFormSet := stringset.NewStringFilter()
	crawler.C.OnHTML(`input[type="file"]`, func(e *colly.HTMLElement) {
		uploadUrl := e.Request.URL.String()
		if !uploadFormSet.Duplicate(uploadUrl) {
			outputFormat := fmt.Sprintf("[upload-form] - %s", uploadUrl)
			fmt.Println(outputFormat)
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}
		}

	})

	// Handle js files
	crawler.C.OnHTML("[src]", func(e *colly.HTMLElement) {
		jsFileUrl := e.Request.AbsoluteURL(e.Attr("src"))
		jsFileUrl = FixUrl(jsFileUrl, crawler.site)
		if jsFileUrl == "" {
			return
		}

		fileExt := GetExtType(jsFileUrl)
		if fileExt == ".js" || fileExt == ".xml" || fileExt == ".json" {
			if !crawler.jsSet.Duplicate(jsFileUrl) {
				outputFormat := fmt.Sprintf("[javascript] - %s", jsFileUrl)
				fmt.Println(outputFormat)
				if crawler.Output != nil {
					crawler.Output.WriteToFile(outputFormat)
				}

				// If JS file is minimal format. Try to find original format
				if strings.Contains(jsFileUrl, ".min.js") {
					originalJS := strings.ReplaceAll(jsFileUrl, ".min.js", ".js")
					_ = crawler.LinkFinderCollector.Visit(originalJS)
				}

				// Send Javascript to Link Finder Collector
				_ = crawler.LinkFinderCollector.Visit(jsFileUrl)
			}
		}
	})

	crawler.C.OnResponse(func(response *colly.Response) {
		respStr := DecodeChars(string(response.Body))
		respLen := len(respStr)

		crawler.findSubdomains(respStr)
		crawler.findAWSS3(respStr)

		// Verify which link is working
		u := response.Request.URL.String()
		outputFormat := fmt.Sprintf("[url] - [code-%d] - [length-%d] - %s", response.StatusCode, respLen, u)
		fmt.Println(outputFormat)
		if crawler.Output != nil {
			crawler.Output.WriteToFile(outputFormat)
		}
	})

	crawler.C.OnError(func(response *colly.Response, err error) {
		Logger.Debugf("Error request: %s - Status code: %v - Error: %s", response.Request.URL.String(), response.StatusCode, err)
		/*
			1xx Informational
			2xx Success
			3xx Redirection
			4xx Client Error
			5xx Server Error
		*/

		if response.StatusCode == 404 || response.StatusCode == 429 || response.StatusCode < 100 || response.StatusCode >= 500 {
			return
		}

		u := response.Request.URL.String()
		outputFormat := fmt.Sprintf("[url] - [code-%d] - %s", response.StatusCode, u)
		fmt.Println(outputFormat)
		if crawler.Output != nil {
			crawler.Output.WriteToFile(outputFormat)
		}
	})

	_ = crawler.C.Visit(crawler.site.String())
}

// Find subdomains from response
func (crawler *Crawler) findSubdomains(resp string) {
	subs := GetSubdomains(resp, crawler.domain)
	for _, sub := range subs {
		if !crawler.subSet.Duplicate(sub) {
			outputFormat := fmt.Sprintf("[subdomains] - %s", sub)
			fmt.Println(outputFormat)
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}
		}
	}
}

// Find AWS S3 from response
func (crawler *Crawler) findAWSS3(resp string) {
	aws := GetAWSS3(resp)
	for _, e := range aws {
		if !crawler.awsSet.Duplicate(e) {
			outputFormat := fmt.Sprintf("[aws-s3] - %s", e)
			fmt.Println(outputFormat)
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}
		}
	}
}

// Setup link finder
func (crawler *Crawler) setupLinkFinder() {
	crawler.LinkFinderCollector.OnResponse(func(response *colly.Response) {
		if response.StatusCode != 200 {
			return
		}

		respStr := string(response.Body)

		crawler.findAWSS3(respStr)
		crawler.findSubdomains(respStr)

		paths, err := LinkFinder(respStr)
		if err != nil {
			Logger.Error(err)
			return
		}

		var inScope bool
		if InScope(response.Request.URL, crawler.C.URLFilters) {
			inScope = true
		}
		for _, path := range paths {
			// JS Regex Result
			outputFormat := fmt.Sprintf("[linkfinder] - [from: %s] - %s", response.Request.URL.String(), path)
			fmt.Println(outputFormat)
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}

			// Try to request JS path
			// Try to generate URLs with main site
			urlWithMainSite := FixUrl(path, crawler.site)
			if urlWithMainSite != "" {
				_ = crawler.C.Visit(urlWithMainSite)
			}

			// Try to generate URLs with the site where Javascript file host in (must be in main or sub domain)
			if inScope {
				urlWithJSHostIn := FixUrl(path, response.Request.URL)
				if urlWithJSHostIn != "" {
					_ = crawler.C.Visit(urlWithJSHostIn)
				}
			}
		}
	})
}
