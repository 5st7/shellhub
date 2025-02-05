package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	resty "github.com/go-resty/resty/v2"
	"github.com/gorilla/websocket"
	"github.com/shellhub-io/shellhub/pkg/models"
	"github.com/shellhub-io/shellhub/pkg/revdial"
	"github.com/shellhub-io/shellhub/pkg/wsconnadapter"
	log "github.com/sirupsen/logrus"
)

const (
	apiHost   = "ssh.shellhub.io"
	apiPort   = 80
	apiScheme = "https"
)

type Client interface {
	commonAPI
	publicAPI
}

type publicAPI interface {
	GetInfo(agentVersion string) (*models.Info, error)
	Endpoints() (*models.Endpoints, error)
	AuthDevice(req *models.DeviceAuthRequest) (*models.DeviceAuthResponse, error)
	NewReverseListener(token string) (*revdial.Listener, error)
	AuthPublicKey(req *models.PublicKeyAuthRequest, token string) (*models.PublicKeyAuthResponse, error)
}

func (c *client) GetInfo(agentVersion string) (*models.Info, error) {
	var info *models.Info

	_, err := c.http.R().
		SetResult(&info).
		Get(buildURL(c, "/info?agent_version="+agentVersion))
	if err != nil {
		return nil, err
	}

	return info, nil
}

func (c *client) AuthDevice(req *models.DeviceAuthRequest) (*models.DeviceAuthResponse, error) {
	var res *models.DeviceAuthResponse
	_, err := c.http.R().
		AddRetryCondition(func(r *resty.Response, err error) bool {
			identity := func(mac, hostname string) string {
				if mac != "" {
					return mac
				}

				return hostname
			}

			log.WithFields(log.Fields{
				"tenant_id":   req.TenantID,
				"identity":    identity(req.Identity.MAC, req.Hostname),
				"status_code": r.StatusCode(),
			}).Debug("failed to authenticate device")

			return r.IsError()
		}).
		SetBody(req).
		SetResult(&res).
		Post(buildURL(c, "/api/devices/auth"))
	if err != nil {
		return nil, err
	}

	return res, nil
}

func (c *client) Endpoints() (*models.Endpoints, error) {
	var endpoints *models.Endpoints
	_, err := c.http.R().
		SetResult(&endpoints).
		Get(buildURL(c, "/endpoints"))
	if err != nil {
		return nil, err
	}

	return endpoints, nil
}

// Dial creates a websocket connection to ShellHub's SSH server.
//
// It receivees the endpoint to connect and the necessary headers for authentication on the server. If the server
// redirect the connection with status [http.StatusTemporaryRedirect] or [http.StatusPermanentRedirect], the Dial method
// will follow. Any other response from the server will result in an error as result of this function.
func Dial(url string, header http.Header) (*websocket.Conn, *http.Response, error) {
	conn, res, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		switch res.StatusCode {
		case http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			location, err := res.Location()
			if err != nil {
				return nil, nil, err
			}

			return Dial(parseToWS(location.String()), header)
		default:
			return nil, nil, err
		}
	}

	return conn, res, nil
}

func (c *client) NewReverseListener(token string) (*revdial.Listener, error) {
	req := c.http.R()
	req.SetHeader("Authorization", fmt.Sprintf("Bearer %s", token))
	req.URL = parseToWS(buildURL(c, "/ssh/connection"))

	conn, res, err := Dial(req.URL, req.Header)
	if err != nil {
		return nil, err
	}

	listener := revdial.NewListener(wsconnadapter.New(conn),
		func(ctx context.Context, path string) (*websocket.Conn, *http.Response, error) {
			return tunnelDial(ctx, strings.Replace(res.Request.URL.Scheme, "http", "ws", 1), res.Request.URL.Hostname(), path)
		},
	)

	return listener, nil
}

func (c *client) AuthPublicKey(req *models.PublicKeyAuthRequest, token string) (*models.PublicKeyAuthResponse, error) {
	var res *models.PublicKeyAuthResponse
	_, err := c.http.R().
		SetBody(req).
		SetResult(&res).
		SetAuthToken(token).
		Post(buildURL(c, "/api/auth/ssh"))
	if err != nil {
		return nil, err
	}

	return res, nil
}

func tunnelDial(ctx context.Context, protocol, address string, path string) (*websocket.Conn, *http.Response, error) {
	getPortFromProtocol := func(protocol string) int {
		if protocol == "wss" {
			return 443
		}

		return 80
	}

	return websocket.DefaultDialer.DialContext(ctx, strings.Join([]string{fmt.Sprintf("%s://%s:%d", protocol, address, getPortFromProtocol(protocol)), path}, ""), nil)
}
