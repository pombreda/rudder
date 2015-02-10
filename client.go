package rudder

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/Unknwon/com"
)

var (
	ErrInvalidEndpoint   = errors.New("endpoint is not valid")
	ErrConnectionRefused = errors.New("connectin is refused")
)

const _USER_AGENT = "dockercn/rudder"

// Client represents a Docker API client.
type Client struct {
	HTTPClient *http.Client
	TLSConfig  *tls.Config

	endpoint    string
	endpointURL *url.URL
}

func parseEndpoint(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, ErrInvalidEndpoint
	}
	if u.Scheme == "tcp" {
		_, port, err := net.SplitHostPort(u.Host)
		if err != nil {
			if e, ok := err.(*net.AddrError); ok {
				if e.Err == "missing port in address" {
					return u, nil
				}
			}
			return nil, ErrInvalidEndpoint
		}
		if com.StrTo(port).MustInt() == 2376 {
			u.Scheme = "https"
		} else {
			u.Scheme = "http"
		}
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "unix" {
		return nil, ErrInvalidEndpoint
	}
	if u.Scheme != "unix" {
		_, port, err := net.SplitHostPort(u.Host)
		if err != nil {
			if e, ok := err.(*net.AddrError); ok {
				if e.Err == "missing port in address" {
					return u, nil
				}
			}
			return nil, ErrInvalidEndpoint
		}
		number := com.StrTo(port).MustInt()
		if number > 0 && number < 65536 {
			return u, nil
		}
	} else {
		return u, nil // we don't need port when using a unix socket
	}
	return nil, ErrInvalidEndpoint
}

// NewClient returns a Client instance ready for communication with the given
// server endpoint.
func NewClient(endpoint string) (*Client, error) {
	u, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	return &Client{
		HTTPClient:  http.DefaultClient,
		endpoint:    endpoint,
		endpointURL: u,
	}, nil
}

// NewTLSClient returns a Client instance ready for TLS communications with the givens
// server endpoint, key and certificates.
func NewTLSClient(endpoint, cert, key, ca string) (*Client, error) {
	u, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if len(cert) == 0 || len(key) == 0 {
		return nil, errors.New("both cert and key path are required")
	}
	tlsCert, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	if ca == "" {
		tlsConfig.InsecureSkipVerify = true
	} else {
		cert, err := ioutil.ReadFile(ca)
		if err != nil {
			return nil, err
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(cert) {
			return nil, errors.New("could not add RootCA pem")
		}
		tlsConfig.RootCAs = caPool
	}
	tr := &http.Transport{
		TLSClientConfig: tlsConfig,
	}
	if err != nil {
		return nil, err
	}
	return &Client{
		HTTPClient:  &http.Client{Transport: tr},
		TLSConfig:   tlsConfig,
		endpoint:    endpoint,
		endpointURL: u,
	}, nil
}

func (c *Client) getURL(path string) string {
	urlStr := strings.TrimRight(c.endpointURL.String(), "/")
	if c.endpointURL.Scheme == "unix" {
		urlStr = ""
	}
	return fmt.Sprintf("%s%s", urlStr, path)
}

// Error represents failures in the API. It represents a failure from the API.
type Error struct {
	Status  int
	Message string
}

func newError(status int, body []byte) *Error {
	return &Error{Status: status, Message: string(body)}
}
func (e *Error) Error() string {
	return fmt.Sprintf("API error (%d): %s", e.Status, e.Message)
}

type jsonMessage struct {
	Status   string `json:"status,omitempty"`
	Progress string `json:"progress,omitempty"`
	Error    string `json:"error,omitempty"`
	Stream   string `json:"stream,omitempty"`
}

func (c *Client) stream(method, path string, setRawTerminal, rawJSONStream bool, headers map[string]string, in io.Reader, stdout, stderr io.Writer) error {
	log.Printf("%s %s", method, path)

	if (method == "POST" || method == "PUT") && in == nil {
		in = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.getURL(path), in)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", _USER_AGENT)
	if method == "POST" {
		req.Header.Set("Content-Type", "plain/text")
	}
	for key, val := range headers {
		req.Header.Set(key, val)
	}
	var resp *http.Response
	protocol := c.endpointURL.Scheme
	address := c.endpointURL.Path
	if stdout == nil {
		stdout = ioutil.Discard
	}
	if stderr == nil {
		stderr = ioutil.Discard
	}
	if protocol == "unix" {
		dial, err := net.Dial(protocol, address)
		if err != nil {
			return err
		}
		clientconn := httputil.NewClientConn(dial, nil)
		resp, err = clientconn.Do(req)
		defer clientconn.Close()
	} else {
		resp, err = c.HTTPClient.Do(req)
	}
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return ErrConnectionRefused
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		return newError(resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Type") == "application/json" {
		// if we want to get raw json stream, just copy it back to output
		// without decoding it
		if rawJSONStream {
			_, err = io.Copy(stdout, resp.Body)
			return err
		}
		dec := json.NewDecoder(resp.Body)
		for {
			var m jsonMessage
			if err := dec.Decode(&m); err == io.EOF {
				break
			} else if err != nil {
				return err
			}
			if m.Stream != "" {
				fmt.Fprint(stdout, m.Stream)
			} else if m.Progress != "" {
				fmt.Fprintf(stdout, "%s %s\r", m.Status, m.Progress)
			} else if m.Error != "" {
				return errors.New(m.Error)
			}
			if m.Status != "" {
				fmt.Fprintln(stdout, m.Status)
			}
		}
	} else {
		if setRawTerminal {
			_, err = io.Copy(stdout, resp.Body)
		} else {
			_, err = stdCopy(stdout, stderr, resp.Body)
		}
		return err
	}
	return nil
}

func queryString(opts interface{}) string {
	if opts == nil {
		return ""
	}
	value := reflect.ValueOf(opts)
	if value.Kind() == reflect.Ptr {
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return ""
	}
	items := url.Values(map[string][]string{})
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		if field.PkgPath != "" {
			continue
		}
		key := field.Tag.Get("qs")
		if key == "" {
			key = strings.ToLower(field.Name)
		} else if key == "-" {
			continue
		}
		v := value.Field(i)
		switch v.Kind() {
		case reflect.Bool:
			if v.Bool() {
				items.Add(key, "1")
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if v.Int() > 0 {
				items.Add(key, strconv.FormatInt(v.Int(), 10))
			}
		case reflect.Float32, reflect.Float64:
			if v.Float() > 0 {
				items.Add(key, strconv.FormatFloat(v.Float(), 'f', -1, 64))
			}
		case reflect.String:
			if v.String() != "" {
				items.Add(key, v.String())
			}
		case reflect.Ptr:
			if !v.IsNil() {
				if b, err := json.Marshal(v.Interface()); err == nil {
					items.Add(key, string(b))
				}
			}
		case reflect.Map:
			if len(v.MapKeys()) > 0 {
				if b, err := json.Marshal(v.Interface()); err == nil {
					items.Add(key, string(b))
				}
			}
		}
	}
	return items.Encode()
}
