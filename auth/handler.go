package auth

/*
 * Microservice gateway application
 * Copyright (C) 2015  Martin Helmich <m.helmich@mittwald.de>
 *                     Mittwald CM Service GmbH & Co. KG
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/gomodule/redigo/redis"
	"github.com/mittwald/servicegateway/config"
	"github.com/op/go-logging"
	cache "github.com/patrickmn/go-cache"
	"github.com/robertkrimen/otto"
)

type AuthenticationHandler struct {
	config      *config.GlobalAuth
	storage     TokenStore
	tokenReader TokenReader
	httpClient  *http.Client
	logger      *logging.Logger
	verifier    *JwtVerifier

	hookPreAuth *otto.Script

	expCache *cache.Cache

	jsVM *otto.Otto
}

type JWTResponse struct {
	JWT                 string
	AllowedApplications []string
}

func NewAuthenticationHandler(
	cfg *config.GlobalAuth,
	redisPool *redis.Pool,
	tokenStore TokenStore,
	verifier *JwtVerifier,
	logger *logging.Logger,
) (*AuthenticationHandler, error) {
	handler := AuthenticationHandler{
		config:      cfg,
		storage:     tokenStore,
		tokenReader: &BearerTokenReader{store: tokenStore},
		httpClient:  &http.Client{},
		logger:      logger,
		verifier:    verifier,
		expCache:    cache.New(cache.NoExpiration, 5*time.Minute),
	}

	if cfg.ProviderConfig.PreAuthenticationHook != "" {
		handler.jsVM = otto.New()
		err := handler.jsVM.Set(
			"log", func(call otto.FunctionCall) otto.Value {
				format := call.Argument(0).String()
				args := call.ArgumentList[1:]
				values := make([]interface{}, len(args))

				for i := range args {
					values[i], _ = args[i].Export()
				}

				logger.Debugf(format, values...)
				return otto.UndefinedValue()
			},
		)
		if err != nil {
			return nil, err
		}

		script, err := handler.jsVM.Compile(cfg.ProviderConfig.PreAuthenticationHook, nil)
		if err != nil {
			return nil, fmt.Errorf("could not parse JS hook %s: %s", cfg.ProviderConfig.PreAuthenticationHook, err.Error())
		}
		handler.hookPreAuth = script
	}

	return &handler, nil
}

func (h *AuthenticationHandler) Authenticate(username string, password string, additionalBodyProperties map[string]interface{}) (*JWTResponse, error) {
	response := JWTResponse{}

	authRequest := h.config.ProviderConfig.Parameters
	authRequest["username"] = username
	authRequest["password"] = password

	requestURL := h.config.ProviderConfig.Url + "/authenticate"

	if h.hookPreAuth != nil {
		_, err := h.jsVM.Run(h.hookPreAuth)
		if err != nil {
			return nil, err
		}

		export, _ := h.jsVM.Get("exports")
		if !export.IsFunction() {
			return nil, fmt.Errorf("hook script must export a function!")
		}

		hookResult, err := export.Call(otto.UndefinedValue(), username, password, additionalBodyProperties)
		if err != nil {
			return nil, fmt.Errorf("error while calling hook function: %s", err.Error())
		}

		hookResultBool, _ := hookResult.ToBoolean()
		if !hookResultBool {
			return nil, InvalidCredentialsError
		}

		if !hookResult.IsObject() {
			return nil, fmt.Errorf("hook function must return object. is: %s", hookResult.Class())
		}

		hookResultObj := hookResult.Object()

		body, err := hookResultObj.Get("body")
		if err != nil {
			return nil, err
		}
		exportedAuthRequest, _ := body.Export()
		newAuthRequest, ok := exportedAuthRequest.(map[string]interface{})

		if ok {
			for k := range newAuthRequest {
				if ottoValue, ok := newAuthRequest[k].(otto.Value); ok {
					newAuthRequest[k], _ = ottoValue.Export()
				}
			}

			authRequest = newAuthRequest
			h.logger.Debugf("hook mapped authentication request to: %s", authRequest)
		}

		url, err := hookResultObj.Get("url")
		if err != nil {
			return nil, err
		}
		if url.IsString() {
			requestURL = url.String()
			h.logger.Debugf("hook set request URL to: %s", url)
		}

		allowedApps, err := hookResultObj.Get("allowedApplications")
		if err != nil {
			return nil, err
		}
		if allowedApps.IsDefined() {
			exported, _ := allowedApps.Export()
			if l, ok := exported.([]string); ok {
				response.AllowedApplications = l
				h.logger.Debugf("token will be restricted to apps: %s", l)
			}
		}
	}

	jsonString, err := json.Marshal(authRequest)
	if err != nil {
		return nil, err
	}

	redactedAuthRequest := authRequest
	if _, ok := redactedAuthRequest["password"]; ok {
		redactedAuthRequest["password"] = "*REDACTED*"
	}

	debugJsonString, _ := json.Marshal(redactedAuthRequest)

	h.logger.Infof("authenticating user %s", username)
	h.logger.Debugf("authentication request: %s", debugJsonString)

	req, err := http.NewRequest("POST", requestURL, bytes.NewBuffer(jsonString))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/jwt")
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)

		if resp.StatusCode == http.StatusForbidden {
			h.logger.Warningf("invalid credentials for user %s: %s", username, body)
			return nil, InvalidCredentialsError
		} else {
			err := fmt.Errorf("unexpected status code %d for user %s: %s", resp.StatusCode, username, body)
			h.logger.Error(err.Error())
			return nil, err
		}
	}

	if resp.StatusCode == 202 {
		h.logger.Infof("user %s has given correct credentials, but additional authentication factor is required", username)
		responseBodyContentType := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(responseBodyContentType, "application/json") {
			return nil, InvalidResponseBodyContentTypeError{
				ContentType: responseBodyContentType,
			}
		}

		var unmarshalledBody map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&unmarshalledBody); err != nil {
			return nil, err
		}
		return nil, &AuthenticationIncompleteError{
			AdditionalProperties: unmarshalledBody,
		}
	}

	body, _ := io.ReadAll(resp.Body)

	response.JWT = string(body)

	return &response, nil
}

func (h *AuthenticationHandler) IsAuthenticated(req *http.Request) (bool, *JWTResponse, error) {
	token, err := h.tokenReader.TokenFromRequest(req)
	if err == NoTokenError {
		return false, nil, nil
	} else if err != nil {
		h.logger.Warningf("error while reading token from request: %s", err)
		return false, nil, err
	}

	exp, ok := h.expCache.Get(token.JWT)
	var expiry int64
	if ok {
		expiry = exp.(int64)
	}

	if ok && (exp == 0 || expiry > time.Now().Unix()) {
		return true, token, nil
	} else if !ok {
		valid, stdClaims, _, err := h.verifier.VerifyToken(token.JWT)
		if err == nil && valid {
			if stdClaims.ExpiresAt == 0 {
				h.expCache.Set(token.JWT, 0, cache.NoExpiration)
				return true, token, nil
			}

			if stdClaims.ExpiresAt > time.Now().Unix() {
				h.expCache.Set(token.JWT, stdClaims.ExpiresAt, time.Duration(stdClaims.ExpiresAt-time.Now().Unix())*time.Second)

				return true, token, nil
			}
		}

		acceptableErrors := jwt.ValidationErrorExpired | jwt.ValidationErrorSignatureInvalid
		if err != nil {
			switch t := err.(type) {
			case *jwt.ValidationError:
				if t.Errors&acceptableErrors != 0 {
					return false, nil, nil
				}
			}
			return false, nil, err
		}
	}
	return false, nil, nil
}
