package connector

import (
	"archive/tar"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"

	dockerclient "github.com/docker/docker/client"
	gliderssh "github.com/gliderlabs/ssh"
	"github.com/shellhub-io/shellhub/pkg/agent/pkg/osauth"
	"github.com/shellhub-io/shellhub/pkg/agent/server/modes"
	"github.com/shellhub-io/shellhub/pkg/api/client"
	"github.com/shellhub-io/shellhub/pkg/models"
	gossh "golang.org/x/crypto/ssh"
)

// NOTICE: Ensures the Authenticator interface is implemented.
var _ modes.Authenticator = (*Authenticator)(nil)

// Authenticator implements the Authenticator interface when the server is running in connector mode.
type Authenticator struct {
	// api is a client to communicate with the ShellHub's API.
	api client.Client
	// authData is the authentication data received from the API to authenticate the device.
	authData *models.DeviceAuthResponse
	// container is the device name.
	//
	// NOTICE: Uses a pointer for later assignment.
	container *string
	// docker is a client to communicate with the Docker's API.
	docker dockerclient.APIClient
	// osauth is an instance of the OSAuth interface to authenticate the user on the Operating System.
	osauth osauth.OSAuther
}

// NewAuthenticator creates a new instance of Authenticator for the connector mode.
func NewAuthenticator(api client.Client, docker dockerclient.APIClient, authData *models.DeviceAuthResponse, container *string) *Authenticator {
	return &Authenticator{
		api:       api,
		authData:  authData,
		container: container,
		docker:    docker,
		osauth:    new(osauth.OSAuth),
	}
}

// getPasswd return a [io.Reader] for the container's passwd file.
func getPasswd(ctx context.Context, cli dockerclient.APIClient, container string) (io.Reader, error) {
	passwdTar, _, err := cli.CopyFromContainer(ctx, container, "/etc/passwd")
	if err != nil {
		return nil, err
	}

	passwd := tar.NewReader(passwdTar)
	if _, err := passwd.Next(); err != nil {
		return nil, err
	}

	return passwd, nil
}

// Password handles the server's SSH password authentication when server is running in connector mode.
func (a *Authenticator) Password(ctx gliderssh.Context, username string, password string) bool {
	passwd, err := getPasswd(ctx, a.docker, *a.container)
	if err != nil {
		return false
	}

	user, err := a.osauth.LookupUserFromPasswd(username, passwd)
	if err != nil {
		return false
	}

	if user.Password == "" {
		// NOTICE(r): when the user doesn't have password, we block the login.
		return false
	}

	shadowTar, _, err := a.docker.CopyFromContainer(ctx, *a.container, "/etc/shadow")
	if err != nil {
		return false
	}

	shadow := tar.NewReader(shadowTar)
	if _, err := shadow.Next(); err != nil {
		return false
	}

	if !a.osauth.AuthUserFromShadow(username, password, shadow) {
		return false
	}

	// NOTICE: set the osauth.User to the context to be obtained later on.
	ctx.SetValue("user", user)

	return true
}

// PublicKey handles the server's SSH public key authentication when server is running in connector mode.
func (a *Authenticator) PublicKey(ctx gliderssh.Context, username string, key gliderssh.PublicKey) bool {
	passwd, err := getPasswd(ctx, a.docker, *a.container)
	if err != nil {
		return false
	}

	user, err := a.osauth.LookupUserFromPasswd(username, passwd)
	if err != nil {
		return false
	}

	type Signature struct {
		Username  string
		Namespace string
	}

	sig := &Signature{
		Username:  username,
		Namespace: *a.container,
	}

	sigBytes, err := json.Marshal(sig)
	if err != nil {
		return false
	}

	sigHash := sha256.Sum256(sigBytes)

	res, err := a.api.AuthPublicKey(&models.PublicKeyAuthRequest{
		Fingerprint: gossh.FingerprintLegacyMD5(key),
		Data:        string(sigBytes),
	}, a.authData.Token)
	if err != nil {
		return false
	}

	digest, err := base64.StdEncoding.DecodeString(res.Signature)
	if err != nil {
		return false
	}

	cryptoKey, ok := key.(gossh.CryptoPublicKey)
	if !ok {
		return false
	}

	pubCrypto := cryptoKey.CryptoPublicKey()

	pubKey, ok := pubCrypto.(*rsa.PublicKey)
	if !ok {
		return false
	}

	if err = rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, sigHash[:], digest); err != nil {
		return false
	}

	// NOTICE: set the osauth.User to the context to be obtained later on.
	ctx.SetValue("user", user)

	return true
}
