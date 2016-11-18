// Package storage provides clients for Microsoft Azure Storage Services.
package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// SASClient is SAS token based BlobClient object that can interact
// with a blob through sas authentication rather than through signed Auth.
type SASClient struct {
	// HTTPClient is the http.SASClient used to initiate API
	// requests.  If it is nil, http.DefaultClient is used.
	HTTPClient *http.Client

	accountName string
	sasKey      string
	useHTTPS    bool
	baseURL     string
	apiVersion  string
}

// NewBasicSASClient constructs a SASClient with given storage service name and
// key.
func NewBasicSASClient(accountName, sasKey string) (SASClient, error) {
	return NewSASClient(sasKey, accountName, DefaultBaseURL, DefaultAPIVersion, defaultUseHTTPS)
}

// NewSASClient constructs a SASClient. This should be used if the caller wants
// to specify whether to use HTTPS, a specific REST API version or a custom
// storage endpoint than Azure Public Cloud.
func NewSASClient(sasKey, accountName, blobServiceBaseURL, apiVersion string, useHTTPS bool) (SASClient, error) {
	var c SASClient
	if sasKey == "" {
		return c, fmt.Errorf("azure: SAS key is required")
	} else if blobServiceBaseURL == "" {
		return c, fmt.Errorf("azure: base storage service url required")
	}

	return SASClient{
		sasKey:      sasKey,
		accountName: accountName,
		useHTTPS:    useHTTPS,
		baseURL:     blobServiceBaseURL,
		apiVersion:  apiVersion,
	}, nil
}

func (c SASClient) getBaseURL(service string) string {
	scheme := "http"
	if c.useHTTPS {
		scheme = "https"
	}
	host := fmt.Sprintf("%s.%s.%s", c.accountName, service, c.baseURL)

	u := &url.URL{
		Scheme: scheme,
		Host:   host}
	return u.String()
}

func (c SASClient) getEndpoint(service, path string, params url.Values) string {
	u, err := url.Parse(c.getBaseURL(service))
	if err != nil {
		// really should not be happening
		panic(err)
	}

	// API doesn't accept path segments not starting with '/'
	if !strings.HasPrefix(path, "/") {
		path = fmt.Sprintf("/%v", path)
	}

	u.Path = path
	u.RawQuery = params.Encode()
	return u.String()
}

// GetBlobService returns a BlobStorageClient which can operate on the blob
// service of the storage account.
func (c SASClient) GetBlobService() BlobStorageClient {
	return BlobStorageClient{c}
}

// GetAPI returns the version of the api variable
func (c SASClient) GetAPI() string {
	return c.apiVersion
}

func (c SASClient) getStandardHeaders() map[string]string {
	return map[string]string{
		"x-ms-version": c.apiVersion,
		"x-ms-date":    currentTimeRfc1123Formatted(),
	}
}

func (c SASClient) getCanonicalizedAccountName() string {
	// since we may be trying to access a secondary storage account, we need to
	// remove the -secondary part of the storage name
	return strings.TrimSuffix(c.accountName, "-secondary")
}

func (c SASClient) buildCanonicalizedHeader(headers map[string]string) string {
	cm := make(map[string]string)

	for k, v := range headers {
		headerName := strings.TrimSpace(strings.ToLower(k))
		match, _ := regexp.MatchString("x-ms-", headerName)
		if match {
			cm[headerName] = v
		}
	}

	if len(cm) == 0 {
		return ""
	}

	keys := make([]string, 0, len(cm))
	for key := range cm {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	ch := ""

	for i, key := range keys {
		if i == len(keys)-1 {
			ch += fmt.Sprintf("%s:%s", key, cm[key])
		} else {
			ch += fmt.Sprintf("%s:%s\n", key, cm[key])
		}
	}
	return ch
}

func (c SASClient) buildCanonicalizedResourceTable(uri string) (string, error) {
	errMsg := "buildCanonicalizedResourceTable error: %s"
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf(errMsg, err.Error())
	}

	cr := "/" + c.getCanonicalizedAccountName()

	if len(u.Path) > 0 {
		cr += u.EscapedPath()
	}

	return cr, nil
}

func (c SASClient) buildCanonicalizedResource(uri string) (string, error) {
	errMsg := "buildCanonicalizedResource error: %s"
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf(errMsg, err.Error())
	}

	cr := "/" + c.getCanonicalizedAccountName()

	if len(u.Path) > 0 {
		// Any portion of the CanonicalizedResource string that is derived from
		// the resource's URI should be encoded exactly as it is in the URI.
		// -- https://msdn.microsoft.com/en-gb/library/azure/dd179428.aspx
		cr += u.EscapedPath()
	}

	params, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf(errMsg, err.Error())
	}

	if len(params) > 0 {
		cr += "\n"
		keys := make([]string, 0, len(params))
		for key := range params {
			keys = append(keys, key)
		}

		sort.Strings(keys)

		for i, key := range keys {
			if len(params[key]) > 1 {
				sort.Strings(params[key])
			}

			if i == len(keys)-1 {
				cr += fmt.Sprintf("%s:%s", key, strings.Join(params[key], ","))
			} else {
				cr += fmt.Sprintf("%s:%s\n", key, strings.Join(params[key], ","))
			}
		}
	}

	return cr, nil
}

func (c SASClient) buildCanonicalizedString(verb string, headers map[string]string, canonicalizedResource string) string {
	contentLength := headers["Content-Length"]
	if contentLength == "0" {
		contentLength = ""
	}
	canonicalizedString := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s",
		verb,
		headers["Content-Encoding"],
		headers["Content-Language"],
		contentLength,
		headers["Content-MD5"],
		headers["Content-Type"],
		headers["Date"],
		headers["If-Modified-Since"],
		headers["If-Match"],
		headers["If-None-Match"],
		headers["If-Unmodified-Since"],
		headers["Range"],
		c.buildCanonicalizedHeader(headers),
		canonicalizedResource)

	return canonicalizedString
}

func (c SASClient) exec(verb, url string, headers map[string]string, body io.Reader) (*storageResponse, error) {
	req, err := http.NewRequest(verb, url, body)
	if err != nil {
		return nil, errors.New("azure/storage: error creating request: " + err.Error())
	}
	query := req.URL.Query()
	req.URL.RawQuery = c.sasKey[1:] + "&" + query.Encode()
	fmt.Println(req.URL.String())
	if clstr, ok := headers["Content-Length"]; ok {
		// content length header is being signed, but completely ignored by golang.
		// instead we have to use the ContentLength property on the request struct
		// (see https://golang.org/src/net/http/request.go?s=18140:18370#L536 and
		// https://golang.org/src/net/http/transfer.go?s=1739:2467#L49)
		req.ContentLength, err = strconv.ParseInt(clstr, 10, 64)
		if err != nil {
			return nil, err
		}
	}
	for k, v := range headers {
		req.Header.Add(k, v)
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	statusCode := resp.StatusCode
	if statusCode >= 400 && statusCode <= 505 {
		var respBody []byte
		respBody, err = readResponseBody(resp)
		if err != nil {
			return nil, err
		}

		if len(respBody) == 0 {
			// no error in response body
			err = fmt.Errorf("storage: service returned without a response body (%s)", resp.Status)
		} else {
			// response contains storage service error object, unmarshal
			storageErr, errIn := serviceErrFromXML(respBody, resp.StatusCode, resp.Header.Get("x-ms-request-id"))
			if err != nil { // error unmarshaling the error response
				err = errIn
			}
			err = storageErr
		}
		return &storageResponse{
			statusCode: resp.StatusCode,
			headers:    resp.Header,
			body:       ioutil.NopCloser(bytes.NewReader(respBody)), /* restore the body */
		}, err
	}

	return &storageResponse{
		statusCode: resp.StatusCode,
		headers:    resp.Header,
		body:       resp.Body}, nil
}

func (c SASClient) execInternalJSON(verb, url string, headers map[string]string, body io.Reader) (*odataResponse, error) {
	req, err := http.NewRequest(verb, url, body)
	for k, v := range headers {
		req.Header.Add(k, v)
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	respToRet := &odataResponse{}
	respToRet.body = resp.Body
	respToRet.statusCode = resp.StatusCode
	respToRet.headers = resp.Header

	statusCode := resp.StatusCode
	if statusCode >= 400 && statusCode <= 505 {
		var respBody []byte
		respBody, err = readResponseBody(resp)
		if err != nil {
			return nil, err
		}

		if len(respBody) == 0 {
			// no error in response body
			err = fmt.Errorf("storage: service returned without a response body (%d)", resp.StatusCode)
			return respToRet, err
		}
		// try unmarshal as odata.error json
		err = json.Unmarshal(respBody, &respToRet.odata)
		return respToRet, err
	}

	return respToRet, nil
}
