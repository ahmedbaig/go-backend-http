package httpx

import (
	"errors"
	"fmt"
	"net/http"
)

type Code string

type FieldError struct {
	Field   string `json:"field"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

type Error struct {
	Status          int          `json:"-"`
	Code            Code         `json:"code"`
	Message         string       `json:"message"`
	CustomerActions []string     `json:"customerActions,omitempty"`
	SupportActions  []string     `json:"supportActions,omitempty"`
	Fields          []FieldError `json:"fields,omitempty"`
	cause           error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

func (e *Error) Wrap(cause error) *Error {
	c := *e
	c.cause = cause
	return &c
}

func (e *Error) WithMessage(msg string) *Error {
	c := *e
	c.Message = msg
	return &c
}

func (e *Error) WithCustomerActions(a ...string) *Error {
	c := *e
	c.CustomerActions = a
	return &c
}

func (e *Error) WithSupportActions(a ...string) *Error {
	c := *e
	c.SupportActions = a
	return &c
}

func (e *Error) WithFields(f ...FieldError) *Error {
	c := *e
	c.Fields = f
	return &c
}

type Definition struct {
	Status          int
	Message         string
	CustomerActions []string
	SupportActions  []string
}

var catalog = map[Code]Definition{
	"E400-1": {http.StatusBadRequest, "The request is malformed", []string{"Check the request and try again"}, nil},
	"E400-2": {http.StatusBadRequest, "The request failed validation", []string{"Correct the highlighted fields"}, nil},
	"E400-3": {http.StatusBadRequest, "The request body is not valid JSON", []string{"Send a valid JSON body"}, nil},
	"E401-1": {http.StatusUnauthorized, "Authentication is required", []string{"Sign in and try again"}, nil},
	"E401-2": {http.StatusUnauthorized, "The session has expired", []string{"Sign in again"}, nil},
	"E403-1": {http.StatusForbidden, "You do not have permission to do this", []string{"Ask an administrator for access"}, nil},
	"E404-1": {http.StatusNotFound, "The requested resource was not found", nil, nil},
	"E404-2": {http.StatusNotFound, "No route matches this request", nil, nil},
	"E405":   {http.StatusMethodNotAllowed, "This method is not allowed on this route", nil, nil},
	"E408":   {http.StatusRequestTimeout, "The request took too long", []string{"Try again later"}, nil},
	"E409":   {http.StatusConflict, "The request conflicts with the current state", []string{"Refresh and try again"}, nil},
	"E410":   {http.StatusGone, "The resource is no longer available", nil, nil},
	"E413":   {http.StatusRequestEntityTooLarge, "The request body is too large", nil, nil},
	"E415":   {http.StatusUnsupportedMediaType, "Unsupported content type", []string{"Send application/json"}, nil},
	"E429":   {http.StatusTooManyRequests, "Too many requests", []string{"Slow down and try again shortly"}, nil},
	"E500-1": {http.StatusInternalServerError, "Something went wrong", []string{"Try again later"}, []string{"Check the service logs for the request ID"}},
	"E502":   {http.StatusBadGateway, "An upstream service failed", []string{"Try again later"}, []string{"Check the upstream service health"}},
	"E503":   {http.StatusServiceUnavailable, "The service is temporarily unavailable", []string{"Try again later"}, nil},
	"E504":   {http.StatusGatewayTimeout, "An upstream service timed out", []string{"Try again later"}, nil},
}

func Register(code Code, d Definition) {
	if d.Status < 400 || d.Status > 599 {
		panic(fmt.Sprintf("httpx: status %d for %s is not an error status", d.Status, code))
	}
	catalog[code] = d
}

func Lookup(code Code) (Definition, bool) {
	d, ok := catalog[code]
	return d, ok
}

func E(code Code) *Error {
	d, ok := catalog[code]
	if !ok {
		d = catalog[CodeInternal]
	}
	return &Error{
		Status:          d.Status,
		Code:            code,
		Message:         d.Message,
		CustomerActions: d.CustomerActions,
		SupportActions:  d.SupportActions,
	}
}

const (
	CodeBadRequest      Code = "E400-1"
	CodeValidation      Code = "E400-2"
	CodeInvalidJSON     Code = "E400-3"
	CodeUnauthenticated Code = "E401-1"
	CodeSessionExpired  Code = "E401-2"
	CodeForbidden       Code = "E403-1"
	CodeNotFound        Code = "E404-1"
	CodeRouteNotFound   Code = "E404-2"
	CodeMethod          Code = "E405"
	CodeTimeout         Code = "E408"
	CodeConflict        Code = "E409"
	CodeGone            Code = "E410"
	CodeTooLarge        Code = "E413"
	CodeMediaType       Code = "E415"
	CodeRateLimited     Code = "E429"
	CodeInternal        Code = "E500-1"
	CodeBadGateway      Code = "E502"
	CodeUnavailable     Code = "E503"
	CodeGatewayTimeout  Code = "E504"
)

func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return E(CodeInternal).Wrap(err)
}
