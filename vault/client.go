/*
 * This file is part of easyKV.
 * Based on code from confd.
 * https://github.com/kelseyhightower/confd/blob/2cacfab234a5d61be4cd88b9e97bee44437c318d/backends/vault/client.go
 * Users who have contributed to this file
 * © 2013 Kelsey Hightower
 *
 * © 2016 The easyKV Authors
 *
 * For the full copyright and license information, please view the LICENSE
 * file that was distributed with this source code.
 */

package vault

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"path"

	"github.com/HeavyHorst/easykv"
	vaultapi "github.com/hashicorp/vault/api"
)

// Client is a wrapper around the vault client
type Client struct {
	client *vaultapi.Client
}

// get a parameter from a map, panics if no value was found
func getParameter(key string, parameters map[string]string) string {
	value := parameters[key]
	if value == "" {
		// panic if a configuration is missing
		panic(fmt.Sprintf("%s is missing from configuration", key))
	}
	return value
}

// panicToError converts a panic to an error
func panicToError(err *error) {
	if r := recover(); r != nil {
		switch t := r.(type) {
		case string:
			*err = errors.New(t)
		case error:
			*err = t
		default: // panic again if we don't know how to handle
			panic(r)
		}
	}
}

// authenticate with the remote client
func authenticate(c *vaultapi.Client, authType string, params map[string]string) (err error) {
	var secret *vaultapi.Secret

	// handle panics gracefully by creating an error
	// this would happen when we get a parameter that is missing
	defer panicToError(&err)

	switch authType {
	case "approle":
		secret, err = c.Logical().Write("/auth/approle/login", map[string]interface{}{
			"role_id":   getParameter("role-id", params),
			"secret_id": getParameter("secret-id", params),
		})
	case "app-id":
		secret, err = c.Logical().Write("/auth/app-id/login", map[string]interface{}{
			"app_id":  getParameter("app-id", params),
			"user_id": getParameter("user-id", params),
		})
	case "github":
		secret, err = c.Logical().Write("/auth/github/login", map[string]interface{}{
			"token": getParameter("token", params),
		})
	case "token":
		c.SetToken(getParameter("token", params))
		secret, err = c.Logical().Read("/auth/token/lookup-self")
	case "userpass":
		username, password := getParameter("username", params), getParameter("password", params)
		secret, err = c.Logical().Write(fmt.Sprintf("/auth/userpass/login/%s", username), map[string]interface{}{
			"password": password,
		})
	case "kubernetes":
		jwt, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
		if err != nil {
			return err
		}
		secret, err = c.Logical().Write("/auth/kubernetes/login", map[string]interface{}{
			"jwt":  string(jwt[:]),
			"role": getParameter("role-id", params),
		})
	case "cert":
		secret, err = c.Logical().Write("/auth/cert/login", nil)
	}

	if err != nil {
		return err
	}

	// if the token has already been set
	if c.Token() != "" {
		return nil
	}

	// the default place for a token is in the auth section
	// otherwise, the backend will set the token itself
	c.SetToken(secret.Auth.ClientToken)
	return nil
}

func getConfig(address, cert, key, caCert string) (*vaultapi.Config, error) {
	conf := vaultapi.DefaultConfig()
	conf.Address = address

	tlsConfig := &tls.Config{}
	if cert != "" && key != "" {
		clientCert, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{clientCert}
		tlsConfig.BuildNameToCertificate()
	}

	if caCert != "" {
		ca, err := ioutil.ReadFile(caCert)
		if err != nil {
			return nil, err
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(ca)
		tlsConfig.RootCAs = caCertPool
	}

	conf.HttpClient.Transport = &http.Transport{
		TLSClientConfig: tlsConfig,
	}

	return conf, nil
}

// New returns an *vault.Client with a connection to named machines.
// It returns an error if a connection to the cluster cannot be made.
func New(address, authType string, opts ...Option) (*Client, error) {
	var options Options
	for _, o := range opts {
		o(&options)
	}

	params := map[string]string{
		"role-id":   options.RoleID,
		"secret-id": options.SecretID,
		"app-id":    options.AppID,
		"user-id":   options.UserID,
		"username":  options.Auth.Username,
		"password":  options.Auth.Password,
		"token":     options.Token,
		"cert":      options.TLS.ClientCert,
		"key":       options.TLS.ClientKey,
		"caCert":    options.TLS.ClientCaKeys,
	}

	if authType == "" {
		return nil, errors.New("you have to set the auth type when using the vault backend")
	}
	conf, err := getConfig(address, options.TLS.ClientCert, options.TLS.ClientKey, options.TLS.ClientCaKeys)

	if err != nil {
		return nil, err
	}

	c, err := vaultapi.NewClient(conf)
	if err != nil {
		return nil, err
	}

	if err := authenticate(c, authType, params); err != nil {
		return nil, err
	}
	return &Client{c}, nil
}

// Close is only meant to fulfill the easykv.ReadWatcher interface.
// Does nothing.
func (c *Client) Close() {}

// GetValues is used to lookup all keys with a prefix.
// Several prefixes can be specified in the keys array.
func (c *Client) GetValues(keys []string) (map[string]string, error) {
	branches := make(map[string]bool)

	for _, key := range keys {
		walkTree(c.client, key, branches)
	}

	vars := make(map[string]string)
	for key := range branches {
		resp, err := c.client.Logical().Read(key)

		if err != nil {
			return nil, err
		}
		if resp == nil || resp.Data == nil {
			continue
		}

		// if the key has only one string value
		// treat it as a string and not a map of values
		if val, ok := isKV(resp.Data); ok {
			vars[key] = val
		} else {
			// save the json encoded response
			// and flatten it to allow usage of gets & getvs
			js, _ := json.Marshal(resp.Data)
			vars[key] = string(js)
			flatten(key, resp.Data, vars)
			delete(vars, key)
		}
	}
	return vars, nil
}

// recursively walk the branches in the Vault, adding to branches map
func walkTree(c *vaultapi.Client, key string, branches map[string]bool) error {
	// strip trailing slash as long as it's not the only character
	if last := len(key) - 1; last > 0 && key[last] == '/' {
		key = key[:last]
	}

	if branches[key] {
		// already processed this branch
		return nil
	}
	branches[key] = true

	resp, err := c.Logical().List(key)
	if err != nil {
		return err
	}
	if resp == nil || resp.Data == nil || resp.Data["keys"] == nil {
		return nil
	}

	switch resp.Data["keys"].(type) {
	case []interface{}:
		// expected
	default:
		return nil
	}

	keyList := resp.Data["keys"].([]interface{})
	for _, innerKey := range keyList {
		switch innerKey.(type) {
		case string:
			innerKey = path.Join(key, "/", innerKey.(string))
			walkTree(c, innerKey.(string), branches)
		}
	}
	return nil
}

// isKV checks if a given map has only one key of type string
// if so, returns the value of that key
func isKV(data map[string]interface{}) (string, bool) {
	if len(data) == 1 {
		if value, ok := data["value"]; ok {
			if text, ok := value.(string); ok {
				return text, true
			}
		}
	}
	return "", false
}

// recursively walks on all the values of a specific key and set them in the variables map
func flatten(key string, value interface{}, vars map[string]string) {
	switch value.(type) {
	case string:
		vars[key] = value.(string)
	case map[string]interface{}:
		inner := value.(map[string]interface{})
		for innerKey, innerValue := range inner {
			innerKey = path.Join(key, "/", innerKey)
			flatten(innerKey, innerValue, vars)
		}
	}
}

// WatchPrefix - not implemented at the moment
func (c *Client) WatchPrefix(ctx context.Context, prefix string, opts ...easykv.WatchOption) (uint64, error) {
	return 0, easykv.ErrWatchNotSupported
}
