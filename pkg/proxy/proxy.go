// Copyright Jetstack Ltd. See LICENSE for details.
package proxy

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/proxy"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/request/bearertoken"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/plugin/pkg/authenticator/token/oidc"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"k8s.io/klog"

	"github.com/gorilla/websocket"
	"github.com/jetstack/kube-oidc-proxy/cmd/app/options"
	"github.com/jetstack/kube-oidc-proxy/pkg/proxy/audit"
	"github.com/jetstack/kube-oidc-proxy/pkg/proxy/context"
	"github.com/jetstack/kube-oidc-proxy/pkg/proxy/hooks"
	"github.com/jetstack/kube-oidc-proxy/pkg/proxy/logging"
	"github.com/jetstack/kube-oidc-proxy/pkg/proxy/subjectaccessreview"
	"github.com/jetstack/kube-oidc-proxy/pkg/proxy/tokenreview"
)

const (
	UserHeaderClientIPKey = "Remote-Client-IP"
	timestampLayout       = "2006-01-02T15:04:05-0700"
)

var (
	errUnauthorized          = errors.New("Unauthorized")
	errNoName                = errors.New("No name in OIDC info")
	errNoImpersonationConfig = errors.New("No impersonation configuration in context")
)

type Config struct {
	DisableImpersonation bool
	TokenReview          bool

	FlushInterval   time.Duration
	ExternalAddress string

	ExtraUserHeaders                map[string][]string
	ExtraUserHeadersClientIPEnabled bool
}

type errorHandlerFn func(http.ResponseWriter, *http.Request, error)

type Proxy struct {
	oidcRequestAuther     *bearertoken.Authenticator
	tokenAuther           authenticator.Token
	tokenReviewer         *tokenreview.TokenReview
	subjectAccessReviewer *subjectaccessreview.SubjectAccessReview
	secureServingInfo     *server.SecureServingInfo
	auditor               *audit.Audit

	restConfig            *rest.Config
	clientTransport       http.RoundTripper
	noAuthClientTransport http.RoundTripper

	config *Config

	hooks       *hooks.Hooks
	handleError errorHandlerFn
}

// implement oidc.CAContentProvider to load
// the ca file from the options
type CAFromFile struct {
	CAFile string
}
type errorResponderWrapper struct {
	errorHandlerFn
}

func (e errorResponderWrapper) Error(w http.ResponseWriter, r *http.Request, err error) {
	e.errorHandlerFn(w, r, err)
}

func (caFromFile CAFromFile) CurrentCABundleContent() []byte {
	res, _ := ioutil.ReadFile(caFromFile.CAFile)
	return res
}

func New(restConfig *rest.Config,
	oidcOptions *options.OIDCAuthenticationOptions,
	auditOptions *options.AuditOptions,
	tokenReviewer *tokenreview.TokenReview,
	subjectAccessReviewer *subjectaccessreview.SubjectAccessReview,
	ssinfo *server.SecureServingInfo,
	config *Config) (*Proxy, error) {

	// load the CA from the file listed in the options
	caFromFile := CAFromFile{
		CAFile: oidcOptions.CAFile,
	}

	// generate tokenAuther from oidc config
	tokenAuther, err := oidc.New(oidc.Options{
		CAContentProvider:    caFromFile,
		ClientID:             oidcOptions.ClientID,
		GroupsClaim:          oidcOptions.GroupsClaim,
		GroupsPrefix:         oidcOptions.GroupsPrefix,
		IssuerURL:            oidcOptions.IssuerURL,
		RequiredClaims:       oidcOptions.RequiredClaims,
		SupportedSigningAlgs: oidcOptions.SigningAlgs,
		UsernameClaim:        oidcOptions.UsernameClaim,
		UsernamePrefix:       oidcOptions.UsernamePrefix,
	})
	if err != nil {
		return nil, err
	}

	auditor, err := audit.New(auditOptions, config.ExternalAddress, ssinfo)
	if err != nil {
		return nil, err
	}

	return &Proxy{
		restConfig:            restConfig,
		hooks:                 hooks.New(),
		tokenReviewer:         tokenReviewer,
		subjectAccessReviewer: subjectAccessReviewer,
		secureServingInfo:     ssinfo,
		config:                config,
		oidcRequestAuther:     bearertoken.New(tokenAuther),
		tokenAuther:           tokenAuther,
		auditor:               auditor,
	}, nil
}

func (p *Proxy) Run(stopCh <-chan struct{}) (<-chan struct{}, <-chan struct{}, error) {
	// standard round tripper for proxy to API Server
	clientRT, err := p.roundTripperForRestConfig(p.restConfig)
	if err != nil {
		return nil, nil, err
	}
	p.clientTransport = clientRT

	// No auth round tripper for no impersonation
	if p.config.DisableImpersonation || p.config.TokenReview {
		noAuthClientRT, err := p.roundTripperForRestConfig(&rest.Config{
			APIPath: p.restConfig.APIPath,
			Host:    p.restConfig.Host,
			Timeout: p.restConfig.Timeout,
			TLSClientConfig: rest.TLSClientConfig{
				CAFile: p.restConfig.CAFile,
				CAData: p.restConfig.CAData,
			},
		})
		if err != nil {
			return nil, nil, err
		}

		p.noAuthClientTransport = noAuthClientRT
	}

	// get API server url
	url, err := url.Parse(p.restConfig.Host)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse url: %s", err)
	}

	p.handleError = p.newErrorHandler()

	// Set up proxy handler using proxy
	proxyHandler := httputil.NewSingleHostReverseProxy(url)
	proxyHandler.Transport = p
	proxyHandler.ErrorHandler = p.handleError
	proxyHandler.FlushInterval = p.config.FlushInterval

	// Set up WebSocket proxy handler
	wsProxyHandler := proxy.NewUpgradeAwareHandler(url, p.clientTransport, true, false, errorResponderWrapper{p.handleError})

	waitCh, listenerStoppedCh, err := p.serve(proxyHandler, wsProxyHandler, stopCh)
	if err != nil {
		return nil, nil, err
	}

	return waitCh, listenerStoppedCh, nil
}

func (p *Proxy) serve(handler http.Handler, wsHandler http.Handler, stopCh <-chan struct{}) (<-chan struct{}, <-chan struct{}, error) {
	// Setup proxy handlers
	handler = p.withHandlers(handler)

	// Add WebSocket handler
	handler = p.withWebSocketHandler(handler, wsHandler)

	// Run auditor
	if err := p.auditor.Run(stopCh); err != nil {
		return nil, nil, err
	}

	// securely serve using serving config
	waitCh, listenerStoppedCh, err := p.secureServingInfo.Serve(handler, time.Second*60, stopCh)
	if err != nil {
		return nil, nil, err
	}

	return waitCh, listenerStoppedCh, nil
}

func (p *Proxy) withWebSocketHandler(handler http.Handler, wsHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if websocket.IsWebSocketUpgrade(req) {
			// Extract additional protocol
			protocols := websocket.Subprotocols(req)
			var authData string
			for _, protocol := range protocols {
				if strings.HasPrefix(protocol, "base64url.bearer.authorization.k8s.io.") {
					authData = strings.TrimPrefix(protocol, "base64url.bearer.authorization.k8s.io.")
					break
				}
			}

			// Decode base64 auth data
			decodedAuthData, err := base64.StdEncoding.DecodeString(authData)
			if err != nil {
				log.Printf("Error decoding base64 auth data: %v", err)
				return
			}
			// Set authorization header
			req.Header.Set("Authorization", string(decodedAuthData))

			// Get the impersonation headers from the context.
			impersonationConf := context.ImpersonationConfig(req)
			log.Printf("ImpersonationConfig: %v", impersonationConf.ImpersonationConfig)
			fmt.Printf("[%s] Handling WS Request, Header: %s\n", time.Now().Format(timestampLayout), req.Header)
			wsHandler.ServeHTTP(rw, req)
		} else {
			handler.ServeHTTP(rw, req)
		}
	})
}

// RoundTrip is called last and is used to manipulate the forwarded request using context.
func (p *Proxy) RoundTrip(req *http.Request) (*http.Response, error) {
	// Here we have successfully authenticated so now need to determine whether
	// we need use impersonation or not.

	// If no impersonation then we return here without setting impersonation
	// header but re-introduce the token we removed.
	if context.NoImpersonation(req) {
		token := context.BearerToken(req)
		req.Header.Add("Authorization", token)
		return p.noAuthClientTransport.RoundTrip(req)
	}

	// Get the impersonation headers from the context.
	impersonationConf := context.ImpersonationConfig(req)
	if impersonationConf == nil {
		return nil, errNoImpersonationConfig
	}

	// Set up impersonation request.
	rt := transport.NewImpersonatingRoundTripper(*impersonationConf.ImpersonationConfig, p.clientTransport)
	//log.Printf("ImpersonationConfig: %v", *impersonationConf.ImpersonationConfig)
	// Log the request
	logging.LogSuccessfulRequest(req, *impersonationConf.InboundUser, *impersonationConf.ImpersonatedUser)

	// Push request through round trippers to the API server.
	return rt.RoundTrip(req)
}

func (p *Proxy) reviewToken(rw http.ResponseWriter, req *http.Request) bool {
	var remoteAddr string
	req, remoteAddr = context.RemoteAddr(req)

	klog.V(4).Infof("attempting to validate a token in request using TokenReview endpoint(%s)",
		remoteAddr)

	ok, err := p.tokenReviewer.Review(req)
	if err != nil {
		klog.Errorf("unable to authenticate the request via TokenReview due to an error (%s): %s",
			remoteAddr, err)
		return false
	}

	if !ok {
		klog.V(4).Infof("passing request with valid token through (%s)",
			remoteAddr)

		return false
	}

	// No error and ok so passthrough the request
	return true
}

func (p *Proxy) roundTripperForRestConfig(config *rest.Config) (http.RoundTripper, error) {
	// get golang tls config to the API server
	tlsConfig, err := rest.TLSConfigFor(config)
	if err != nil {
		return nil, err
	}

	// create tls transport to request
	tlsTransport := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: tlsConfig,
	}

	// get kube transport config form rest client config
	restTransportConfig, err := config.TransportConfig()
	if err != nil {
		return nil, err
	}

	// wrap golang tls config with kube transport round tripper
	clientRT, err := transport.HTTPWrappersForConfig(restTransportConfig, tlsTransport)
	if err != nil {
		return nil, err
	}

	return clientRT, nil
}

// Return the proxy OIDC token authenticator
func (p *Proxy) OIDCTokenAuthenticator() authenticator.Token {
	return p.tokenAuther
}

func (p *Proxy) RunPreShutdownHooks() error {
	return p.hooks.RunPreShutdownHooks()
}
