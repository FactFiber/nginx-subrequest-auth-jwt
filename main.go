package main

import (
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/carlpett/nginx-auth-jwt/logger"

	"github.com/dgrijalva/jwt-go"
	"github.com/dgrijalva/jwt-go/request"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/yaml.v2"
)

var (
	requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of http requests handled",
	}, []string{"status"})
	validationTime = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "nginx_subrequest_auth_jwt_token_validation_time_seconds",
		Help:    "Number of seconds spent validating token",
		Buckets: prometheus.ExponentialBuckets(100*time.Nanosecond.Seconds(), 3, 6),
	})
)

const (
	claimsSourceStatic      = "static"
	claimsSourceQueryString = "queryString"
)

func init() {
	requestsTotal.WithLabelValues("200")
	requestsTotal.WithLabelValues("401")
	requestsTotal.WithLabelValues("405")
	requestsTotal.WithLabelValues("500")

	prometheus.MustRegister(
		requestsTotal,
		validationTime,
	)
}

type server struct {
	PublicKey       *ecdsa.PublicKey
	Logger          logger.Logger
	ClaimsSource    string
	StaticClaims    []map[string][]string
	CookieNames     []string
	ResponseHeaders map[string]string
}

func newServer(logger logger.Logger, configFilePath string) (*server, error) {
	cfg, err := ioutil.ReadFile(configFilePath)
	if err != nil {
		return nil, err
	}

	var config config
	err = yaml.Unmarshal(cfg, &config)
	if err != nil {
		return nil, err
	}
	var keyMaterial string
	if config.ValidationKeys[0].KeyFrom != nil {
		keyFrom := config.ValidationKeys[0].KeyFrom
		if keyFrom.Source != "env" {
			return nil, fmt.Errorf(
				"keyFrom source unknown: %s", keyFrom.Source)
		}
		keyMaterial = os.Getenv(keyFrom.Name)
	} else {
		keyMaterial = config.ValidationKeys[0].KeyMaterial
	}

	// TODO: Only supports a single EC PubKey for now
	pubkey, err := jwt.ParseECPublicKeyFromPEM([]byte(keyMaterial))
	if err != nil {
		return nil, err
	}

	if !contains([]string{"static", "queryString"}, config.ClaimsSource) {
		return nil, fmt.Errorf("claimsSource parameter must be set and either 'static' or 'queryString'")
	}

	if config.ClaimsSource == claimsSourceStatic && len(config.StaticClaims) == 0 {
		return nil, fmt.Errorf("Claims configuration is empty")
	}

	return &server{
		PublicKey:       pubkey,
		Logger:          logger,
		ClaimsSource:    config.ClaimsSource,
		StaticClaims:    config.StaticClaims,
		CookieNames:     config.CookieNames,
		ResponseHeaders: config.ResponseHeaders,
	}, nil
}

type validationKey struct {
	Type        string     `yaml:"type"`
	KeyMaterial string     `yaml:"key,omitempty"`
	KeyFrom     *keySource `yaml:"keyFrom,omitempty"`
}

type keySource struct {
	Source string `yaml:"source"`
	Name   string `yaml:"name"`
	Value  string `yaml:"-"`
}

type config struct {
	ValidationKeys  []validationKey       `yaml:"validationKeys"`
	ClaimsSource    string                `yaml:"claimsSource"`
	StaticClaims    []map[string][]string `yaml:"claims"`
	CookieNames     []string              `yaml:"cookieNames"`
	ResponseHeaders map[string]string     `yaml:"responseHeaders"`
}

var (
	configFilePath = kingpin.Flag("config", "Path to configuration file").Default("config.yaml").ExistingFile()
	logLevel       = kingpin.Flag("log-level", "Log level").Default("info").Enum("debug", "info", "warn", "error", "fatal")

	tlsKey   = kingpin.Flag("tls-key", "Path to TLS key").ExistingFile()
	tlsCert  = kingpin.Flag("tls-cert", "Path to TLS cert").ExistingFile()
	bindAddr = kingpin.Flag("addr", "Address/port to serve traffic in TLS mode").Default(":8443").String()

	insecure         = kingpin.Flag("insecure", "Serve traffic unencrypted over http (default false)").Bool()
	insecureBindAddr = kingpin.Flag("insecure-addr", "Address/port to serve traffic in insecure mode").Default(":8080").String()
)

func main() {
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	logger := logger.NewLogger(*logLevel)

	server, err := newServer(logger, *configFilePath)
	if err != nil {
		logger.Fatalw("Couldn't initialize server", "err", err)
	}

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/validate", server.validate)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "OK") })

	if *insecure {
		logger.Infow("Starting server", "addr", *insecureBindAddr)
		err = http.ListenAndServe(*insecureBindAddr, nil)
	} else {
		logger.Infow("Starting server", "addr", *bindAddr)
		if *tlsKey == "" || *tlsCert == "" {
			logger.Fatalw("tls-key and tls-cert are required in TLS mode")
		}
		err = http.ListenAndServeTLS(*bindAddr, *tlsCert, *tlsKey, nil)
	}
	if err != nil {
		logger.Fatalw("Error running server", "err", err)
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(b)
}

func (s *server) validate(rw http.ResponseWriter, r *http.Request) {
	w := &statusWriter{ResponseWriter: rw}
	defer func() {
		if r := recover(); r != nil {
			s.Logger.Errorw("Recovered panic", "err", r)
			requestsTotal.WithLabelValues("500").Inc()
			w.WriteHeader(http.StatusInternalServerError)
		}
		s.Logger.Debugw("Handled validation request", "url", r.URL, "status", w.status, "method", r.Method, "userAgent", r.UserAgent())
	}()

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.Logger.Infow("Invalid method", "method", r.Method)
		requestsTotal.WithLabelValues("405").Inc()
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	claims, ok := s.validateDeviceToken(r)
	if !ok {
		requestsTotal.WithLabelValues("401").Inc()
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	requestsTotal.WithLabelValues("200").Inc()
	s.writeResponseHeaders(w, r, claims)
	w.WriteHeader(http.StatusOK)
}

func (s *server) validateDeviceToken(
	r *http.Request,
) (claims jwt.MapClaims, ok bool) {
	t := time.Now()
	defer validationTime.Observe(time.Since(t).Seconds())

	var xCookie = CookieExtractor(s.CookieNames)
	var extractor = request.MultiExtractor{
		&xCookie,
		request.AuthorizationHeaderExtractor}
	token, err := request.ParseFromRequest(
		r, extractor,
		func(token *jwt.Token) (interface{}, error) {
			// TODO: Only supports EC for now
			if _, ok := token.Method.(*jwt.SigningMethodECDSA); !ok {
				return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
			}
			return s.PublicKey, nil
		}, request.WithClaims(&claims))
	if err != nil {
		s.Logger.Debugw("Failed to parse token", "err", err)
		return nil, false
	}
	if !token.Valid {
		s.Logger.Debugw("Invalid token", "token", token.Raw)
		return nil, false
	}
	if err := claims.Valid(); err != nil {
		s.Logger.Debugw("Got invalid claims", "err", err)
		return nil, false
	}

	switch s.ClaimsSource {
	case claimsSourceStatic:
		ok = s.staticClaimValidator(claims)
	case claimsSourceQueryString:
		ok = s.queryStringClaimValidator(claims, r)
	default:
		s.Logger.Errorw("Configuration error: Unhandled claims source", "claimsSource", s.ClaimsSource)
		return nil, false
	}
	if !ok {
		return nil, false
	}
	return claims, true
}

// CookieExtractor is list of cookies to look for token in.
type CookieExtractor []string

// ExtractToken returns token in matching cookie.
// implements request.Extractor interface
func (x *CookieExtractor) ExtractToken(
	req *http.Request,
) (string, error) {
	for _, cookie := range req.Cookies() {
		if contains(([]string)(*x), cookie.Name) {
			return cookie.Value, nil
		}
	}
	return "", request.ErrNoTokenInRequest
}

func (s *server) staticClaimValidator(claims jwt.MapClaims) bool {
	var valid bool
	for _, claimSet := range s.StaticClaims {
		valid = true
		for claimName, validValues := range claimSet {
			if !s.checkClaim(claimName, validValues, claims) {
				valid = false
				break
			}
		}
		if valid {
			break
		}
	}

	if !valid {
		s.Logger.Debugw("Token claims did not match required values", "validClaims", s.StaticClaims, "actualClaims", claims)
	}
	return valid
}

func (s *server) queryStringClaimValidator(claims jwt.MapClaims, r *http.Request) bool {
	validClaims := r.URL.Query()
	hasClaimsPrefixedKey := false
	for key := range validClaims {
		if strings.HasPrefix(key, "claims_") {
			hasClaimsPrefixedKey = true
		}
	}
	if len(validClaims) == 0 || !hasClaimsPrefixedKey {
		s.Logger.Warnw("No claims requirements sent, rejecting", "queryParams", validClaims)
		return false
	}
	s.Logger.Debugw("Validating claims from query string", "validClaims", validClaims)

	passedValidation := true
	for claimNameQ, validValues := range validClaims {
		if strings.HasPrefix(claimNameQ, "claims_") {
			claimName := strings.TrimPrefix(claimNameQ, "claims_")
			s.Logger.Debugw("CLAIM", "claim", claimName, "vv", validValues,
				"qd", validClaims)
			if !s.checkClaim(claimName, validValues, claims) {
				passedValidation = false
				break
			}
		}
	}

	if !passedValidation {
		s.Logger.Debugw("Token claims did not match required values", "validClaims", validClaims, "actualClaims", claims)
	}
	return passedValidation
}

func (s *server) checkClaim(
	claimName string, validValues []string, claims jwt.MapClaims,
) bool {
	actual, ok := claims[claimName].(string)
	if !ok {
		actualList, ok := claims[claimName].([]interface{})
		if !ok {
			s.Logger.Infow(
				"Claims list unknown structure", "claims",
				fmt.Sprintf("%+v", claims))
			return false
		}
		found := false
		for _, actual := range actualList {
			if contains(validValues, actual.(string)) {
				found = true
				break
			}
		}
		if !found {
			s.Logger.Debugw(
				"Rejecting claim", "claimName", claimName,
				"validValues", validValues, "actual", actualList)
			return false
		}
	} else if !contains(validValues, actual) {
		s.Logger.Debugw(
			"Rejecting claim", "claimName", claimName,
			"validValues", validValues, "actual", actual)
		return false
	}
	return true
}

func (s *server) writeResponseHeaders(
	w *statusWriter, r *http.Request, claims jwt.MapClaims,
) {
	parameters := r.URL.Query()
	for key, value := range parameters {
		if strings.HasPrefix(key, "responses_") {
			header := strings.TrimPrefix(key, "responses_")
			s.ResponseHeaders[header] = value[0]
		}
	}
	s.Logger.Debugw("responseHeaders", "rh", s.ResponseHeaders)
	if s.ResponseHeaders == nil {
		return
	}
	for header, claimName := range s.ResponseHeaders {
		claim, ok := claims[claimName]
		if !ok {
			continue
		}
		var toClaim []byte
		if sClaim, ok := claim.(string); ok {
			toClaim = ([]byte)(sClaim)
		} else {
			var err error
			toClaim, err = json.Marshal(claim)
			if err != nil {
				continue
			}
		}
		encClaim := base64.StdEncoding.EncodeToString(toClaim)
		s.Logger.Debugw("add response header", "header", header, "claim", claim, "encClaim", encClaim)
		w.Header().Add(header, encClaim)
	}
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
