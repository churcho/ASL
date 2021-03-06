package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
	"html"
	"html/template"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	authorization           = "authorization"
	fadalaxAuthHeader       = "x-fadalax-auth"
	fadalaxCertSerialHeader = "x-fadalax-serial"
	fadalaxAuthRegex        = `^CN=([[:alnum:]]+)@fadalax\.tech$`
	emailRegex              = "^[a-zA-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$"
)

var hydraAdminURL = flag.String("admin-url", "https://localhost:9001", "URL of the hydra admin api")
var listen = flag.String("listen", ":8088", "on what url to start the server on")
var dsn = flag.String("dsn", "", "DSN of the DB to connect to: user:password@/dbname")
var vaultURL = flag.String("vault-url", "https://vault.fadalax.tech:8200", "URL of the Vault instance")
var issuer = flag.String("issuer", "https://hydra.fadalax.tech:9000/", "OpenID Connect issuer")
var clientID = flag.String("clientID", "fadalax-frontend", "Client id")

type server struct {
	router          *mux.Router
	auth            TokenValidator
	hydra           hydraAdminClient
	db              storageClient
	vault           vaultClient
	templateLogin   *template.Template
	templateConsent *template.Template
}

type hydraAdminClient interface {
	GetLoginInfo(challenge string) (LoginInfo, error)
	AcceptLogin(challenge string, req AcceptLoginRequest) (AcceptLoginResponse, error)
	GetConsentInfo(challenge string) (ConsentInfo, error)
	AcceptConsent(challenge string, req AcceptConsentRequest) (AcceptConsentResponse, error)
}

type storageClient interface {
	GetUser(ctx context.Context, userID string) (User, error)
	ChangePassword(ctx context.Context, userID string, password string) error
	Login(ctx context.Context, userID string, password string) bool
	EditUser(ctx context.Context, user User) error
}

type TokenValidator interface {
	// Validate returns the uid if the token in authHeader is valid, an error otherwise.
	Validate(ctx context.Context, authHeader string) (string, error)
}

type vaultClient interface {
	PKIRoleExists(role string) (bool, error)
	CreatePKIUser(name string) error
	CertificateIsValid(pkiMount, serial string) (bool, error)
}

func main() {
	log.SetLevel(log.TraceLevel) // log all the things
	flag.Parse()
	// Setting up client to communicate with hydra
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	hydra := HydraClient{client: &http.Client{Transport: tr}, adminUrl: *hydraAdminURL}
	if *dsn == "" {
		log.Error("Empty DSN passed.")
	}
	db, err := NewStorage(*dsn)
	if err != nil {
		log.WithError(err).Fatal("Failed to create storage component.")
	}
	auth, err := NewValidator(*issuer, *clientID)
	if err != nil {
		log.WithError(err).Fatal("Failed to create token validation component.")
	}
	// Reads token from VAULT_TOKEN automatically.
	vc, err := NewVaultClient(*vaultURL, "")
	if err != nil {
		log.WithError(err).Fatal("Failed to create vault client.")
	}

	// Prepare HTTP server
	r := mux.NewRouter()
	ser := server{hydra: &hydra, router: r, db: db, vault: vc, auth: auth}

	// Prepare template
	ser.templateLogin, err = template.ParseFiles("./template/login.html")
	if err != nil {
		log.WithError(err).Fatal("Failed to parse login template.")
	}
	ser.templateConsent, err = template.ParseFiles("./template/consent.html")
	if err != nil {
		log.WithError(err).Fatal("Failed to parse consent template.")
	}

	r.HandleFunc("/login", ser.Login)
	r.HandleFunc("/consent", ser.Consent)
	r.HandleFunc("/cert", ser.IssueCert).Methods(http.MethodGet)
	r.HandleFunc("/cert", ser.RevokeCert).Methods(http.MethodDelete)
	r.HandleFunc("/user", ser.GetUser).Methods(http.MethodGet)
	r.HandleFunc("/user", ser.EditUser).Methods(http.MethodPut)
	r.HandleFunc("/user/password", ser.EditPw).Methods(http.MethodPut)
	// Kind of a smoke test.
	u, err := ser.db.GetUser(context.Background(), "a3")
	if err != nil {
		log.WithError(err).Fatalf("Failed to execute known good query")
	} else {
		log.WithField("user", u).Info("Found user.")
	}
	// Setup CORS
	h := handlers.CORS(handlers.AllowedOriginValidator(func(o string) bool {
		return strings.HasSuffix(o, "fadalax.tech")
	}), handlers.AllowedMethods([]string{
		http.MethodGet,
		http.MethodPut,
		http.MethodPost,
		http.MethodOptions,
		http.MethodDelete,
	}), handlers.AllowedHeaders([]string{"Authorization", "Content-Type", "Content-Disposition"}),
		handlers.AllowCredentials())(r)
	// Run
	log.Fatal(http.ListenAndServe(*listen, h))
}

func (s server) Login(w http.ResponseWriter, r *http.Request) {
	l := log.WithContext(r.Context())
	l.Debugf("%s, %q", r.Method, html.EscapeString(r.URL.Path))
	//keys[0] contains the challenge
	keys, ok := r.URL.Query()["login_challenge"]
	if !ok {
		l.Info("No login challenge provided")
		s.httpBadRequest(w, "no login challenge provided")
		return
	}
	info, err := s.hydra.GetLoginInfo(keys[0])
	if err != nil {
		l.Error("Error getting login info", "error", err)
		s.httpInternalError(w, err)
		return
	}

	authenticated := info.Skip
	username := info.Subject

	if r.Method == http.MethodGet && !info.Skip {
		authHeader := r.Header.Get(fadalaxAuthHeader)
		regex := regexp.MustCompile(fadalaxAuthRegex)
		foundMatch := false
		for _, hdr := range strings.Split(authHeader, ",") {
			ms := regex.FindStringSubmatch(hdr)
			if len(ms) != 2 {
				log.Errorf("length not 2: %v %v", hdr, ms)
				continue
			}
			if username != "" && username != ms[1] {
				log.Error("wrong username: %v != %v", ms[1], username)
				continue
			}
			foundMatch = true
			username = ms[1]
			break
		}
		if foundMatch {
			certSerial := r.Header.Get(fadalaxCertSerialHeader)
			if certSerial == "" {
				log.Warn("Empty certificate serial passed.")
				s.httpUnauthorized(w)
				return
			}
			// serial needs to be split AA:BB:CC
			if !strings.ContainsRune(certSerial, ':') && !strings.ContainsRune(certSerial, '-') {
				if len(certSerial)%2 != 0 {
					s.httpUnauthorized(w)
					return
				}
				elems := make([]string, len(certSerial)/2)
				for i := 0; i < len(certSerial); i += 2 {
					elems[i/2] = certSerial[i : i+2]
				}
				certSerial = strings.Join(elems, ":")
			}
			pkiMount := fmt.Sprintf("pki-user/%s", username)
			if username == "admin" {
				pkiMount = "pki"
			}
			authenticated, err = s.vault.CertificateIsValid(pkiMount, certSerial)
			if err != nil {
				log.WithError(err).WithField("serial", certSerial).Error("Failed to ask vault whether the certificate has been revoked.")
				s.httpUnauthorized(w)
				return
			}
		}

		// Cert auth failed, show login
		if !authenticated {
			err := s.templateLogin.Execute(w, map[string]interface{}{})
			if err != nil {
				s.httpInternalError(w, err)
			}
			return
		}
	}

	if r.Method == http.MethodPost {
		err = r.ParseForm()
		if err != nil {
			s.httpBadRequest(w, "invalid form")
			return
		}
		username = r.FormValue("username")
		password := r.FormValue("password")
		l = l.WithField("username", username)
		authenticated = s.db.Login(r.Context(), username, password)
		l.Info("Login Attempt.")
	}

	// Accept login request
	if authenticated {
		l.Info("Authenticated")
		acceptBody := AcceptLoginRequest{Subject: username, Remember: false, RememberFor: 300}
		accRes, err := s.hydra.AcceptLogin(keys[0], acceptBody)
		if err != nil {
			l.WithError(err).Error("Error accepting login.")
			s.httpInternalError(w, err)
			return
		}

		exists, err := s.vault.PKIRoleExists(username)
		if err != nil {
			l.WithError(err).Error("Failed to check whether a PKI role exists.")
			s.httpInternalError(w, err) // TODO(bimmlerd) do we leak too much information here?
			return
		}

		if !exists {
			err := s.vault.CreatePKIUser(username)
			if err != nil {
				l.WithError(err).Error("Failed to create PKI User.")
				s.httpInternalError(w, err) // TODO(bimmlerd) do we leak too much information here?
				return
			}
		}

		// redirect
		http.Redirect(w, r, accRes.RedirectTo, http.StatusFound)
		return
	}
	s.httpUnauthorized(w)
}

func (s server) Consent(w http.ResponseWriter, r *http.Request) {
	log.Debugf("%s, %q", r.Method, html.EscapeString(r.URL.Path))
	//keys[0] contains the challenge
	keys, ok := r.URL.Query()["consent_challenge"]
	if !ok {
		log.Info("No consent challenge provided")
		s.httpBadRequest(w, "no login challenge provided")
		return
	}
	challenge := keys[0]

	//fetch information about the request
	cinfo, err := s.hydra.GetConsentInfo(challenge)
	if err != nil {
		log.WithError(err).Error("Error getting consent info")
		s.httpInternalError(w, err)
		return
	}
	consent := cinfo.Skip

	if r.Method == http.MethodGet && !cinfo.Skip {
		err := s.templateConsent.Execute(w, map[string]interface{}{})
		if err != nil {
			s.httpInternalError(w, err)
		}
		return
	}
	if r.Method == http.MethodPost {
		//TODO: check, whether the user gave consent and user should give consent if not...
		consent = true
	}

	if consent {
		requestBody := AcceptConsentRequest{GrantScope: cinfo.RequestedScope, GrantAccessTokenAudience: cinfo.RequestedAudience, Remember: true, RememberFor: 300}
		conRes, err := s.hydra.AcceptConsent(keys[0], requestBody)
		if err != nil {
			log.WithError(err).Error("Error giving consent.")
			s.httpInternalError(w, err)
			return
		}
		http.Redirect(w, r, conRes.RedirectTo, http.StatusFound)
		return
	}
	s.httpUnauthorized(w)
}

func (s server) GetUser(w http.ResponseWriter, r *http.Request) {
	log.Debugf("%s, %q", r.Method, html.EscapeString(r.URL.Path))
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	h := r.Header.Get(authorization)
	if h == "" {
		log.Warn("Missing authorization header in request to GetUser.")
		s.httpUnauthorized(w)
		return
	}

	id, err := s.auth.Validate(r.Context(), h)
	if err != nil {
		log.WithError(err).Error("Failed to validate authorization token.")
		s.httpUnauthorized(w)
		return
	}

	u, err := s.db.GetUser(ctx, id)
	if err != nil {
		if err == sql.ErrNoRows {
			log.WithField("user-id", id).Warn("User not found.")
			s.httpNotFound(w)
			return
		}
		log.WithError(err).WithField("user-id", id).Error("Failed to GetUser.")
		s.httpInternalError(w, fmt.Errorf("failed to get user"))
		return
	}

	w.Header().Set("content-type", "application/json")
	err = json.NewEncoder(w).Encode(u)
	if err != nil {
		s.httpInternalError(w, err)
	}
}

func (s server) EditUser(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	h := r.Header.Get(authorization)
	if h == "" {
		log.Warn("Missing authorization header in request to GetUser.")
		s.httpUnauthorized(w)
		return
	}

	id, err := s.auth.Validate(r.Context(), h)
	if err != nil {
		log.WithError(err).Error("Failed to validate authorization token.")
		s.httpUnauthorized(w)
		return
	}
	l := log.WithField("uid", id)

	var u User
	reqBody, err := ioutil.ReadAll(r.Body)
	if err != nil || len(reqBody) == 0 {
		l.WithError(err).Error("EditUser request without body")
		s.httpBadRequest(w, "Could not parse body.")
		return
	}
	err = json.Unmarshal(reqBody, &u)
	if err != nil {
		l.WithError(err).Error("EditUser error unmarshaling json")
		s.httpInternalError(w, fmt.Errorf("failed to parse body"))
		return
	}

	if id != u.UserID {
		l.Error("EditUser user id does not match")
		s.httpBadRequest(w, "id does not match id in json object")
		return
	}

	if !regexp.MustCompile(alphanumeric).MatchString(u.UserID) || len(u.UserID) == 0 {
		l.Error("Invalid id format.")
		s.httpBadRequest(w, "Invalid id format")
		return
	}
	if !regexp.MustCompile(alphanumeric).MatchString(u.FirstName) || len(u.FirstName) == 0 {
		l.Error("Invalid first name format.")
		s.httpBadRequest(w, "Invalid first name format")
		return
	}
	if !regexp.MustCompile(alphanumeric).MatchString(u.UserID) || len(u.LastName) == 0 {
		l.Error("Invalid last name format.")
		s.httpBadRequest(w, "Invalid last name format")
		return
	}
	if !regexp.MustCompile(emailRegex).MatchString(u.Email) || len(u.Email) == 0 {
		l.Error("Invalid email format.")
		s.httpBadRequest(w, "Invalid email format")
		return
	}

	err = s.db.EditUser(ctx, u)
	if err != nil {
		if err == sql.ErrNoRows {
			log.WithField("user-id", u.UserID).Warn("user not found")
			s.httpNotFound(w)
			return
		}
		log.WithError(err).WithField("user-id", u.UserID).Error("Failed to edit user.")
		s.httpInternalError(w, fmt.Errorf("failed to edit user"))
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")

}

func (s server) EditPw(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	l := log.WithField("method", "EditPw")
	h := r.Header.Get(authorization)
	if h == "" {
		log.Warn("Missing authorization header in request to GetUser.")
		s.httpUnauthorized(w)
		return
	}

	id, err := s.auth.Validate(r.Context(), h)
	if err != nil {
		log.WithError(err).Error("Failed to validate authorization token.")
		s.httpUnauthorized(w)
		return
	}
	l = l.WithField("uid", id)

	var pw struct {
		Password string `json:"password"`
	}
	reqBody, err := ioutil.ReadAll(r.Body)
	if err != nil || len(reqBody) == 0 {
		l.WithError(err).Error("EditPw request without body")
		s.httpBadRequest(w, "Could not parse body.")
		return
	}
	err = json.Unmarshal(reqBody, &pw)
	if err != nil {
		l.WithError(err).Error("EditPw error unmarshaling json")
		s.httpInternalError(w, fmt.Errorf("failed to parse body"))
		return
	}
	s.db.ChangePassword(ctx, id, pw.Password)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func (s server) IssueCert(w http.ResponseWriter, r *http.Request) {
	log.Debugf("%s, %q", r.Method, html.EscapeString(r.URL.Path))
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	h := r.Header.Get(authorization)
	if h == "" {
		log.Warn("Missing authorization header in request to GetUser.")
		s.httpUnauthorized(w)
		return
	}

	id, err := s.auth.Validate(r.Context(), h)
	if err != nil {
		log.WithError(err).Error("Failed to validate authorization token.")
		s.httpUnauthorized(w)
		return
	}

	vc, err := NewVaultUserClient(*vaultURL, id, h)
	if err != nil {
		log.WithError(err).Error("Failed to create vault client.")
		s.httpUnauthorized(w)
		return
	}

	cert, err := vc.GetCert(ctx, id)
	if err != nil {
		log.WithError(err).Error("Failed to create certificate.")
		s.httpUnauthorized(w)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=cert.p12")
	w.Header().Set("Content-Type", "application/x-pkcs12")

	w.Write(cert)
}

func (s server) RevokeCert(w http.ResponseWriter, r *http.Request) {
	log.Debugf("%s, %q", r.Method, html.EscapeString(r.URL.Path))
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	h := r.Header.Get(authorization)
	if h == "" {
		log.Warn("Missing authorization header in request to GetUser.")
		s.httpUnauthorized(w)
		return
	}

	id, err := s.auth.Validate(r.Context(), h)
	if err != nil {
		log.WithError(err).Error("Failed to validate authorization token.")
		s.httpUnauthorized(w)
		return
	}

	vc, err := NewVaultUserClient(*vaultURL, id, h)
	if err != nil {
		log.WithError(err).Error("Failed to create vault client.")
		s.httpUnauthorized(w)
		return
	}

	err = vc.RevokeCerts(ctx, id)
	if err != nil {
		log.WithError(err).Error("Failed to revoke certificate.")
		s.httpUnauthorized(w)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func (s server) httpInternalError(w http.ResponseWriter, e error) {
	if e != nil {
		log.Errorf("Error: %v\n", e)
	}
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

func (s server) httpNotFound(w http.ResponseWriter) {
	http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
}

func (s server) httpUnauthorized(w http.ResponseWriter) {
	http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
}

func (s server) httpBadRequest(w http.ResponseWriter, error string) {
	http.Error(w, error, http.StatusBadRequest)
}
