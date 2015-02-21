package rudder

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var (
	ErrMissingOutputStream = errors.New("output stream is missing")
	ErrMissingContext      = errors.New("context is missing")
	ErrMultipleContexts    = errors.New("multiple contexts are presented")
)

// Image represents a Docker image.
type Image struct {
}

// AuthConfiguration represents authentication options to use in the PushImage
// method. It represents the authentication in the Docker index server.
type AuthConfiguration struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Email         string `json:"email,omitempty"`
	ServerAddress string `json:"serveraddress,omitempty"`
}

// AuthConfigurations represents authentication options to use for the
// PushImage method accommodating the new X-Registry-Config header
type AuthConfigurations struct {
	Configs map[string]AuthConfiguration `json:"configs"`
}

func headersWithAuth(auths ...interface{}) (map[string]string, error) {
	headers := make(map[string]string)
	for _, auth := range auths {
		data, err := json.Marshal(auth)
		if err != nil {
			return nil, err
		}
		switch auth.(type) {
		case AuthConfiguration:
			headers["X-Registry-Auth"] = base64.URLEncoding.EncodeToString(data)
		case AuthConfigurations:
			headers["X-Registry-Config"] = base64.URLEncoding.EncodeToString(data)
		}
	}
	return headers, nil
}

// BuildImageOptions present the set of informations available for building an image
// from a tarfile with a Dockerfile in it.
type BuildImageOption struct {
	Name                string `qs:"t"`
	SuppressOutput      bool   `qs:"q"`
	NoCache             bool   `qs:"nocache"`
	Pull                bool   `qs:"pull"`
	RmTmpContainer      bool   `qs:"rm"`
	ForceRmTmpContainer bool   `qs:"forcerm"`
	Remote              string `qs:"remote"`

	InputStream   io.Reader          `qs:"-"`
	OutputStream  io.Writer          `qs:"-"`
	RawJSONStream bool               `qs:"-"`
	Auth          AuthConfiguration  `qs:"-"` // for older docker X-Registry-Auth header
	AuthConfigs   AuthConfigurations `qs:"-"` // for newer docker X-Registry-Config header
	ContextDir    string             `qs:"-"`
}

// BuildImage builds an image from a tarball's url or a Dockerfile in the input stream.
//
// http://goo.gl/S5z7PQ
func (c *Client) BuildImage(opt BuildImageOption) error {
	if opt.OutputStream == nil {
		return ErrMissingOutputStream
	}

	headers, err := headersWithAuth(opt.Auth, opt.AuthConfigs)
	if err != nil {
		return fmt.Errorf("marshal header: %v", err)
	}

	if opt.InputStream != nil || len(opt.ContextDir) > 0 {
		headers["Content-Type"] = "application/tar"
	} else {
		return ErrMissingContext
	}
	if len(opt.ContextDir) > 0 {
		if opt.InputStream != nil {
			return ErrMultipleContexts
		}
		if opt.InputStream, err = createTarStream(opt.ContextDir); err != nil {
			return fmt.Errorf("create tar stream: %v", err)
		}
	}

	return c.stream("POST", fmt.Sprintf("/build?%s",
		queryString(&opt)), true, opt.RawJSONStream, headers, opt.InputStream, opt.OutputStream, nil)
}
