package main

import (
	"encoding/json"
	"errors"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
	"github.com/go-chi/chi"
	"github.com/knadh/otpgateway"
	"github.com/stretchr/testify/assert"
)

type dummyProv struct{}

// ID returns the Provider's ID.
func (d *dummyProv) ID() string {
	return dummyProvider
}

// ChannelName returns the e-mail Provider's name.
func (d *dummyProv) ChannelName() string {
	return "dummychannel"
}

// Description returns help text for the e-mail verification Provider.
func (d *dummyProv) Description() string {
	return "dummy description"
}

// ValidateAddress "validates" an e-mail address.
func (d *dummyProv) ValidateAddress(to string) error {
	if to != dummyToAddress {
		return errors.New("invalid dummy to address")
	}
	return nil
}

// Push pushes an e-mail to the SMTP server.
func (d *dummyProv) Push(toAddr string, subject string, m []byte) error {
	return nil
}

// MaxOTPLen returns the maximum allowed length of the OTP value.
func (d *dummyProv) MaxOTPLen() int {
	return 6
}

// MaxBodyLen returns the max permitted body size.
func (d *dummyProv) MaxBodyLen() int {
	return 100 * 1024
}

const (
	dummyNamespace = "myapp"
	dummySecret    = "mysecret"
	dummyProvider  = "dummyprovider"
	dummyOTPID     = "myotp123"
	dummyToAddress = "dummy@to.com"
	dummyOTP       = "123456"
)

var (
	srv  *httptest.Server
	rdis *miniredis.Miniredis
)

func init() {
	// Dummy Redis.
	rd, err := miniredis.Run()
	if err != nil {
		log.Println(err)
	}
	rdis = rd
	port, _ := strconv.Atoi(rd.Port())

	// Provider templates.
	tpl := template.New("dummy")
	tpl, _ = tpl.Parse("test {{ .OTP }}")

	// Dummy app.
	app := &App{
		logger:    logger,
		providers: map[string]otpgateway.Provider{dummyProvider: &dummyProv{}},
		providerTpls: map[string]*providerTpl{
			dummyProvider: &providerTpl{
				subject: tpl,
				tpl:     tpl,
			},
		},
		otpTTL:         10 * time.Second,
		otpMaxAttempts: 3,
		store: otpgateway.NewRedisStore(otpgateway.RedisConf{
			Host: rd.Host(),
			Port: port,
		}),
	}

	authCreds := map[string]string{dummyNamespace: dummySecret}
	r := chi.NewRouter()
	r.Get("/api/providers", auth(authCreds, wrap(app, handleGetProviders)))
	r.Put("/api/otp/{id}", auth(authCreds, wrap(app, handleSetOTP)))
	r.Post("/api/otp/{id}", auth(authCreds, wrap(app, handleCheckOTP)))
	r.Get("/otp/{namespace}/{id}", wrap(app, handleIndex))
	r.Post("/otp/{namespace}/{id}", wrap(app, handleIndex))
	srv = httptest.NewServer(r)
}

func reset() {
	rdis.FlushDB()
}

func TestGetProviders(t *testing.T) {
	var out httpResp
	r := testRequest(t, http.MethodGet, "/api/providers", nil, &out)
	assert.Equal(t, http.StatusOK, r.StatusCode, "non 200 response")
	assert.Equal(t, out.Data, []interface{}{dummyProvider}, "providers don't match")
}
func TestSetOTP(t *testing.T) {
	rdis.FlushDB()
	var (
		data = &otpResp{}
		out  = httpResp{
			Data: data,
		}
		p = url.Values{}
	)
	p.Set("to", dummyToAddress)
	p.Set("provider", "badprovider")

	// Register an OTP with a bad provider.
	r := testRequest(t, http.MethodPut, "/api/otp/"+dummyOTPID, p, &out)
	assert.Equal(t, http.StatusBadRequest, r.StatusCode, "non 400 response for bad provider")

	// Register an OTP with a bad to address.
	p.Set("to", "xxxx")
	r = testRequest(t, http.MethodPut, "/api/otp/"+dummyOTPID, p, &out)
	assert.Equal(t, http.StatusBadRequest, r.StatusCode, "non 400 response for bad to address")

	// Register without ID and OTP.
	p.Set("provider", dummyProvider)
	p.Set("to", dummyToAddress)
	r = testRequest(t, http.MethodPut, "/api/otp/"+dummyOTPID, p, &out)
	assert.Equal(t, http.StatusOK, r.StatusCode, "non 200 response")
	assert.Equal(t, dummyToAddress, data.OTP.To, "to doesn't match")
	assert.Equal(t, 1, data.OTP.Attempts, "attempts doesn't match")
	assert.NotEqual(t, "", data.OTP.ID, "id wasn't auto generated")
	assert.NotEqual(t, "", data.OTP.ID, "otp wasn't auto generated")

	// Register with known data.
	p.Set("id", dummyOTPID)
	p.Set("otp", dummyOTP)
	r = testRequest(t, http.MethodPut, "/api/otp/"+dummyOTPID, p, &out)
	assert.Equal(t, dummyOTPID, data.OTP.ID, "id doesn't match")
	assert.Equal(t, dummyOTP, data.OTP.OTP, "otp doesn't match")
}

func TestCheckOTP(t *testing.T) {
	rdis.FlushDB()
	var (
		data = &otpResp{}
		out  = httpResp{
			Data: data,
		}
		p = url.Values{}
	)
	p.Set("id", dummyOTPID)
	p.Set("otp", dummyOTP)
	p.Set("to", dummyToAddress)
	p.Set("provider", dummyProvider)

	// Register OTP.
	r := testRequest(t, http.MethodPut, "/api/otp/"+dummyOTPID, p, &out)
	assert.Equal(t, http.StatusOK, r.StatusCode, "otp registration failed")

	// Check OTP.
	cp := url.Values{}
	r = testRequest(t, http.MethodPost, "/api/otp/"+dummyOTPID, cp, &out)
	assert.Equal(t, http.StatusBadRequest, r.StatusCode, "non 400 response for empty otp check")

	// Bad OTP.
	cp.Set("otp", "123")
	r = testRequest(t, http.MethodPost, "/api/otp/"+dummyOTPID, cp, &out)
	assert.Equal(t, http.StatusBadRequest, r.StatusCode, "non 400 response for bad otp check")
	assert.Equal(t, 2, data.Attempts, "attempts didn't increase")

	// Good OTP.
	cp.Set("otp", dummyOTP)
	r = testRequest(t, http.MethodPost, "/api/otp/"+dummyOTPID, cp, &data)
	assert.Equal(t, http.StatusOK, r.StatusCode, "good OTP failed")
}

func testRequest(t *testing.T, method, path string, p url.Values, out interface{}) *http.Response {
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(p.Encode()))
	if err != nil {
		t.Fatal(err)
		return nil
	}
	req.SetBasicAuth(dummyNamespace, dummySecret)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	// HTTP client.
	c := &http.Client{}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
		return nil
	}

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
		return nil
	}
	defer resp.Body.Close()

	if err := json.Unmarshal(respBody, out); err != nil {
		t.Fatal(err)
	}

	return resp
}